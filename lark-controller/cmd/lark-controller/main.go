package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/config"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/larkapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/oauthbridge"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/oauthcontract"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/observability"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/webhook"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

const (
	controllerReadHeaderTimeout = 500 * time.Millisecond
	controllerReadTimeout       = 2 * time.Second
	controllerWriteTimeout      = 2300 * time.Millisecond
)

func main() {
	if err := run(); err != nil {
		log.Printf("lark controller stopped: %v", err)
		os.Exit(1)
	}
}

func run() error {
	loaded, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	policyCatalog, err := policy.LoadDirectory(
		loaded.PolicyBundleDirectory,
		loaded.ApprovalBindingsFile,
	)
	if err != nil {
		return err
	}
	if active := policyCatalog.ActivePolicyVersion(); active != loaded.ActivePolicyVersion {
		return fmt.Errorf(
			"configured active policy %q does not match loaded active policy %q",
			loaded.ActivePolicyVersion,
			active,
		)
	}
	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: loaded.AppID, AppSecret: loaded.AppSecret,
	})
	if err != nil {
		return err
	}
	grantKeyring, err := newapi.LoadGrantPayloadKeyringFile(loaded.GrantPayloadKeyringFile)
	if err != nil {
		return err
	}
	grantClient, err := prepareGrantClient(loaded)
	if err != nil {
		return err
	}
	store, err := inbox.Open(loaded.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := store.SyncPolicySnapshot(context.Background(), policyCatalog.Snapshot()); err != nil {
		return err
	}
	operationalHandler, err := observability.NewHandler(
		string(loaded.Mode),
		store,
		loaded.ReadinessMaxQueueAge,
	)
	if err != nil {
		return err
	}
	processor, err := worker.NewShadowProcessorWithGrantSealer(
		store,
		fetcher,
		policyCatalog,
		loaded.Locale,
		grantKeyring,
	)
	if err != nil {
		return err
	}
	eventHandler, err := webhook.NewHandler(webhook.Config{
		VerificationToken: loaded.VerificationToken,
		EncryptKey:        loaded.EventEncryptKey,
		AppID:             loaded.AppID,
		TenantKey:         loaded.TenantKey,
	}, store)
	if err != nil {
		return err
	}
	oauthHandler, err := prepareOAuthBridge(loaded, store)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("POST /integrations/lark/events", eventHandler)
	oauthHandler.Register(mux)
	operationalHandler.Register(mux)
	server := newControllerHTTPServer(loaded.ListenAddress, mux)
	listener, err := net.Listen("tcp", loaded.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for lark controller: %w", err)
	}
	defer func() { _ = listener.Close() }()
	grantRuntime, err := activateGrantRuntime(
		context.Background(),
		loaded.Mode,
		store,
		grantClient,
		grantKeyring,
	)
	if err != nil {
		return err
	}

	rootContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go runWorker(rootContext, "lark event", processor, loaded.WorkerPoll)
	if grantRuntime != nil {
		go runWorker(rootContext, "New API grant", grantRuntime, loaded.WorkerPoll)
	}

	serveResult := make(chan error, 1)
	go func() {
		log.Printf("lark controller listening in %s mode", loaded.Mode)
		serveResult <- server.Serve(listener)
	}()

	select {
	case <-rootContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func prepareOAuthBridge(loaded config.Config, store *inbox.Store) (*oauthbridge.Handler, error) {
	if store == nil {
		return nil, errors.New("OAuth credential store is required")
	}
	if len(loaded.NewAPIOAuthCallbackAllowlist) != 1 {
		return nil, errors.New("exactly one New API OAuth callback is required")
	}
	exchanger, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: loaded.AppID, AppSecret: loaded.AppSecret,
		RedirectURI: oauthcontract.ControllerCallbackURI,
		TenantKey:   loaded.TenantKey,
	})
	if err != nil {
		return nil, err
	}
	return oauthbridge.NewHandler(oauthbridge.Config{
		BridgeClientID:     loaded.BridgeClientID,
		NewAPIRedirectURI:  loaded.NewAPIOAuthCallbackAllowlist[0],
		RateLimitPerMinute: loaded.OAuthRateLimitPerMinute,
		TrustedProxyCIDRs:  loaded.OAuthTrustedProxyCIDRs,
	}, store, exchanger)
}

func prepareGrantClient(loaded config.Config) (worker.GrantClient, error) {
	switch loaded.Mode {
	case config.ModeShadow:
		return nil, nil
	case config.ModeActive:
		secret, err := newapi.LoadIntegrationSecretFile(loaded.IntegrationSecretFile)
		if err != nil {
			return nil, err
		}
		return newapi.NewClient(newapi.Config{
			BaseURL:           loaded.NewAPIBaseURL,
			IntegrationSecret: secret,
		})
	default:
		return nil, errors.New("controller mode must be shadow or active")
	}
}

func activateGrantRuntime(
	ctx context.Context,
	mode config.Mode,
	store *inbox.Store,
	client worker.GrantClient,
	keyring *newapi.GrantKeyring,
) (*worker.ActiveGrantRuntime, error) {
	if store == nil || keyring == nil {
		return nil, errors.New("store and grant keyring are required")
	}
	if err := store.ValidateEntitlementGrantJobKeyIDs(ctx, keyring.KeyIDs()); err != nil {
		return nil, err
	}
	switch mode {
	case config.ModeShadow:
		return nil, nil
	case config.ModeActive:
		executor, err := worker.NewGrantExecutor(store, client, keyring)
		if err != nil {
			return nil, err
		}
		runtime, err := worker.NewActiveGrantRuntime(store, executor)
		if err != nil {
			return nil, err
		}
		if _, err := runtime.ReleaseHeldJobs(ctx); err != nil {
			return nil, err
		}
		return runtime, nil
	default:
		return nil, errors.New("controller mode must be shadow or active")
	}
}

func newControllerHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: controllerReadHeaderTimeout,
		ReadTimeout:       controllerReadTimeout,
		WriteTimeout:      controllerWriteTimeout,
		IdleTimeout:       60 * time.Second,
	}
}

type runOnceWorker interface {
	RunOnce(context.Context) (bool, error)
}

func runWorker(ctx context.Context, name string, processor runOnceWorker, idlePoll time.Duration) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		processed, err := processor.RunOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("%s processing failed", name)
		}
		if processed {
			timer.Reset(0)
		} else {
			timer.Reset(idlePoll)
		}
	}
}
