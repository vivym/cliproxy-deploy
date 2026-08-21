package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/config"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/larkapi"
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
	store, err := inbox.Open(loaded.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := store.SyncPolicySnapshot(context.Background(), policyCatalog.Snapshot()); err != nil {
		return err
	}
	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: loaded.AppID, AppSecret: loaded.AppSecret,
	})
	if err != nil {
		return err
	}
	processor, err := worker.NewShadowProcessor(store, fetcher, policyCatalog, loaded.Locale)
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

	rootContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go runWorker(rootContext, processor, loaded.WorkerPoll)

	mux := http.NewServeMux()
	mux.Handle("POST /integrations/lark/events", eventHandler)
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(response).Encode(map[string]string{"status": "ok", "mode": loaded.Mode})
	})
	server := newControllerHTTPServer(loaded.ListenAddress, mux)
	serveResult := make(chan error, 1)
	go func() {
		log.Printf("lark controller listening in shadow mode")
		serveResult <- server.ListenAndServe()
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

func runWorker(ctx context.Context, processor *worker.ShadowProcessor, idlePoll time.Duration) {
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
			log.Printf("shadow event processing failed")
		}
		if processed {
			timer.Reset(0)
		} else {
			timer.Reset(idlePoll)
		}
	}
}
