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
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/observability"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/webhook"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

const (
	controllerReadHeaderTimeout = 500 * time.Millisecond
	controllerReadTimeout       = 2 * time.Second
	controllerWriteTimeout      = 2300 * time.Millisecond
	larkRequestInterval         = 100 * time.Millisecond
	approvalReconciliationRetry = 5 * time.Minute
	maxApprovalRetryDelay       = 24 * time.Hour
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
	baseSubscription, err := policyCatalog.ResolveBaseSubscription()
	if err != nil {
		return err
	}
	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: loaded.AppID, AppSecret: loaded.AppSecret,
	})
	if err != nil {
		return err
	}
	larkRequestPacer, err := worker.NewRequestPacer(larkRequestInterval)
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
	policySnapshot := policyCatalog.Snapshot()
	if err := store.SyncPolicySnapshot(context.Background(), policySnapshot); err != nil {
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
		VerificationToken:      loaded.VerificationToken,
		EncryptKey:             loaded.EventEncryptKey,
		AppID:                  loaded.AppID,
		TenantKey:              loaded.TenantKey,
		PrincipalDisableSealer: grantKeyring,
	}, store)
	if err != nil {
		return err
	}
	oauthHandler, err := prepareOAuthBridge(loaded, store, baseSubscription, grantKeyring)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("POST /integrations/lark/events", eventHandler)
	oauthHandler.RegisterInternal(mux)
	if loaded.OAuthPublicEnabled {
		oauthHandler.RegisterPublic(mux)
	}
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
		baseSubscription.PolicyVersion,
		grantKeyring,
	)
	if err != nil {
		return err
	}
	principalDisableRuntime, err := activatePrincipalDisableRuntime(
		context.Background(),
		loaded.Mode,
		store,
		grantClient,
		grantKeyring,
	)
	if err != nil {
		return err
	}
	employmentChecker, err := prepareEmploymentChecker(loaded)
	if err != nil {
		return err
	}
	employmentReconciler, err := activateEmploymentReconciliation(
		loaded.Mode,
		store,
		grantClient,
		employmentChecker,
		grantKeyring,
		loaded.TenantKey,
		loaded.ReconciliationHealthOpenID,
		larkRequestPacer,
	)
	if err != nil {
		return err
	}
	approvalReconciler, err := activateApprovalReconciliation(
		loaded.Mode,
		store,
		fetcher,
		fetcher,
		policySnapshot,
		loaded.AppID,
		loaded.TenantKey,
		loaded.Locale,
		loaded.ApprovalReconcileLookback,
		larkRequestPacer,
	)
	if err != nil {
		return err
	}
	processingReconciler, err := worker.NewProcessingReconciler(
		store,
		loaded.ProcessingLeaseTimeout,
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
	if principalDisableRuntime != nil {
		go runWorker(rootContext, "New API principal disable", principalDisableRuntime, loaded.WorkerPoll)
	}
	if employmentReconciler != nil {
		go runScheduledWorker(
			rootContext,
			"Lark employment reconciliation",
			employmentReconciler,
			loaded.ReconciliationInterval,
			15*time.Minute,
		)
	}
	go runScheduledWorker(
		rootContext,
		"Lark approval reconciliation",
		approvalReconciler,
		loaded.ApprovalReconcileInterval,
		approvalReconciliationRetry,
	)
	go runScheduledWorker(
		rootContext,
		"controller processing recovery",
		processingReconciler,
		loaded.ProcessingRecoveryInterval,
		loaded.ProcessingRecoveryInterval,
	)

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

func prepareOAuthBridge(
	loaded config.Config,
	store *inbox.Store,
	baseSubscription policy.BaseSubscriptionResolution,
	grantSealer oauthbridge.GrantRequestSealer,
) (*oauthbridge.Handler, error) {
	if store == nil {
		return nil, errors.New("OAuth credential store is required")
	}
	if len(loaded.NewAPIOAuthCallbackAllowlist) != 1 {
		return nil, errors.New("exactly one New API OAuth callback is required")
	}
	bridgeClientSecret, err := oauthbridge.LoadClientSecretFile(loaded.BridgeClientSecretFile)
	if err != nil {
		return nil, err
	}
	exchanger, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: loaded.AppID, AppSecret: loaded.AppSecret,
		RedirectURI: loaded.ControllerCallbackURI,
		TenantKey:   loaded.TenantKey,
	})
	if err != nil {
		return nil, err
	}
	return oauthbridge.NewHandler(oauthbridge.Config{
		BridgeClientID:     loaded.BridgeClientID,
		BridgeClientSecret: bridgeClientSecret,
		NewAPIRedirectURI:  loaded.NewAPIOAuthCallbackAllowlist[0],
		BaseSubscription: oauthbridge.BaseSubscriptionConfig{
			PolicyVersion: baseSubscription.PolicyVersion,
			LevelCode:     baseSubscription.LevelCode,
			PeriodQuota:   baseSubscription.PeriodQuota,
			ResetPeriod:   baseSubscription.ResetPeriod,
			ResetTimezone: baseSubscription.ResetTimezone,
			CatalogSHA256: baseSubscription.CatalogSHA256,
		},
		RateLimitPerMinute: loaded.OAuthRateLimitPerMinute,
		TrustedProxyCIDRs:  loaded.OAuthTrustedProxyCIDRs,
	}, store, exchanger, grantSealer)
}

type integrationClient interface {
	worker.GrantClient
	worker.PrincipalDisableClient
	worker.ActivePrincipalLister
}

func prepareGrantClient(loaded config.Config) (integrationClient, error) {
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

func prepareEmploymentChecker(loaded config.Config) (worker.EmploymentChecker, error) {
	switch loaded.Mode {
	case config.ModeShadow:
		return nil, nil
	case config.ModeActive:
		return larkapi.NewEmploymentChecker(larkapi.Config{
			AppID: loaded.AppID, AppSecret: loaded.AppSecret,
		})
	default:
		return nil, errors.New("controller mode must be shadow or active")
	}
}

func activateEmploymentReconciliation(
	mode config.Mode,
	store *inbox.Store,
	principalLister worker.ActivePrincipalLister,
	employmentChecker worker.EmploymentChecker,
	sealer worker.PrincipalDisableSealer,
	tenantKey string,
	healthOpenID string,
	requestPacer worker.RequestPacer,
) (*worker.EmploymentReconciler, error) {
	switch mode {
	case config.ModeShadow:
		return nil, nil
	case config.ModeActive:
		return worker.NewEmploymentReconciler(worker.EmploymentReconcilerConfig{
			Store: store, PrincipalLister: principalLister,
			EmploymentChecker: employmentChecker, PrincipalDisableSealer: sealer,
			TenantKey: tenantKey, HealthOpenID: healthOpenID,
			RequestPacer: requestPacer,
		})
	default:
		return nil, errors.New("controller mode must be shadow or active")
	}
}

func activateApprovalReconciliation(
	mode config.Mode,
	store *inbox.Store,
	instanceLister worker.ApprovalInstanceLister,
	instanceFetcher worker.ApprovalFetcher,
	snapshot policy.Snapshot,
	appID string,
	tenantKey string,
	locale string,
	lookback time.Duration,
	requestPacer worker.RequestPacer,
) (*worker.ApprovalReconciler, error) {
	if mode != config.ModeShadow && mode != config.ModeActive {
		return nil, errors.New("controller mode must be shadow or active")
	}
	bindings, err := approvalReconciliationBindings(snapshot)
	if err != nil {
		return nil, err
	}
	return worker.NewApprovalReconciler(worker.ApprovalReconcilerConfig{
		Store: store, InstanceLister: instanceLister, InstanceFetcher: instanceFetcher,
		Bindings: bindings, AppID: appID, TenantKey: tenantKey, Locale: locale,
		InitialLookback: lookback, RequestPacer: requestPacer,
	})
}

func approvalReconciliationBindings(
	snapshot policy.Snapshot,
) ([]worker.ApprovalReconciliationBinding, error) {
	policyStates := make(map[string]policy.PolicyState, len(snapshot.Policies))
	for _, item := range snapshot.Policies {
		if item.PolicyVersion == "" {
			return nil, errors.New("approval reconciliation policy version is required")
		}
		if _, duplicate := policyStates[item.PolicyVersion]; duplicate {
			return nil, fmt.Errorf("duplicate approval reconciliation policy %q", item.PolicyVersion)
		}
		policyStates[item.PolicyVersion] = item.State
	}
	bindings := make([]worker.ApprovalReconciliationBinding, 0, len(snapshot.Bindings))
	for _, binding := range snapshot.Bindings {
		state, exists := policyStates[binding.PolicyVersion]
		if !exists {
			return nil, fmt.Errorf(
				"approval reconciliation binding %q references an unknown policy",
				binding.ApprovalCode,
			)
		}
		scanBinding := worker.ApprovalReconciliationBinding{ApprovalCode: binding.ApprovalCode}
		switch state {
		case policy.PolicyStateActive:
			if binding.AcceptInstanceStartedBefore != "" {
				return nil, fmt.Errorf("active approval %q has a scan cutoff", binding.ApprovalCode)
			}
		case policy.PolicyStateDraining:
			cutoff, err := time.Parse(time.RFC3339, binding.AcceptInstanceStartedBefore)
			if err != nil {
				return nil, fmt.Errorf("draining approval %q requires a valid scan cutoff", binding.ApprovalCode)
			}
			scanBinding.ScanUntil = cutoff
		case policy.PolicyStateRetired:
			continue
		default:
			return nil, fmt.Errorf("approval %q has an invalid policy state", binding.ApprovalCode)
		}
		bindings = append(bindings, scanBinding)
	}
	return bindings, nil
}

func activatePrincipalDisableRuntime(
	ctx context.Context,
	mode config.Mode,
	store *inbox.Store,
	client worker.PrincipalDisableClient,
	keyring *newapi.GrantKeyring,
) (*worker.ActivePrincipalDisableRuntime, error) {
	if store == nil || keyring == nil {
		return nil, errors.New("store and grant keyring are required")
	}
	if err := validateIntegrationPayloadKeyIDs(ctx, store, keyring); err != nil {
		return nil, err
	}
	switch mode {
	case config.ModeShadow:
		return nil, nil
	case config.ModeActive:
		executor, err := worker.NewPrincipalDisableExecutor(store, client, keyring)
		if err != nil {
			return nil, err
		}
		runtime, err := worker.NewActivePrincipalDisableRuntime(store, executor)
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

func activateGrantRuntime(
	ctx context.Context,
	mode config.Mode,
	store *inbox.Store,
	client worker.GrantClient,
	activeBasePolicyVersion string,
	keyring *newapi.GrantKeyring,
) (*worker.ActiveGrantRuntime, error) {
	if store == nil || keyring == nil {
		return nil, errors.New("store and grant keyring are required")
	}
	if err := validateIntegrationPayloadKeyIDs(ctx, store, keyring); err != nil {
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
		runtime, err := worker.NewActiveGrantRuntime(store, executor, activeBasePolicyVersion)
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

func validateIntegrationPayloadKeyIDs(
	ctx context.Context,
	store *inbox.Store,
	keyring *newapi.GrantKeyring,
) error {
	keyIDs := keyring.KeyIDs()
	if err := store.ValidateEntitlementGrantJobKeyIDs(ctx, keyIDs); err != nil {
		return err
	}
	return store.ValidatePrincipalDisableJobKeyIDs(ctx, keyIDs)
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

func runScheduledWorker(
	ctx context.Context,
	name string,
	processor runOnceWorker,
	interval time.Duration,
	retryInterval time.Duration,
) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		_, err := processor.RunOnce(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("%s processing failed", name)
			}
		}
		timer.Reset(scheduledWorkerDelay(err, interval, retryInterval))
	}
}

func scheduledWorkerDelay(err error, interval time.Duration, retryInterval time.Duration) time.Duration {
	if err == nil {
		return interval
	}
	delay := retryInterval
	var employmentFailure *worker.EmploymentCheckError
	if errors.As(err, &employmentFailure) && employmentFailure != nil &&
		employmentFailure.Retryable && employmentFailure.RetryAfter > delay {
		delay = employmentFailure.RetryAfter
	}
	var approvalFailure *worker.ApprovalFetchError
	if errors.As(err, &approvalFailure) && approvalFailure != nil {
		if approvalFailure.Retryable && approvalFailure.RetryAfter > delay {
			delay = approvalFailure.RetryAfter
		}
		if delay > maxApprovalRetryDelay {
			delay = maxApprovalRetryDelay
		}
		return delay
	}
	if delay > interval && retryInterval <= interval {
		return interval
	}
	return delay
}
