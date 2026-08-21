package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/webhook"
)

func TestWebhookAcknowledgementBudgetIncludesHeaderReadAndInboxContention(t *testing.T) {
	if controllerReadHeaderTimeout+controllerWriteTimeout >= 3*time.Second {
		t.Fatalf("server acknowledgement budget = %s, want less than 3s",
			controllerReadHeaderTimeout+controllerWriteTimeout)
	}
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	locker, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: databasePath}).String())
	if err != nil {
		t.Fatalf("open lock connection: %v", err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	lockConnection, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	t.Cleanup(func() { _ = lockConnection.Close() })
	if _, err := lockConnection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("lock database for writing: %v", err)
	}
	t.Cleanup(func() { _, _ = lockConnection.ExecContext(context.Background(), "ROLLBACK") })

	eventHandler, err := webhook.NewHandler(webhook.Config{
		VerificationToken: "verification-token",
		AppID:             "cli_test",
		TenantKey:         "tenant-test",
	}, store)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /integrations/lark/events", eventHandler)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := newControllerHTTPServer(listener.Addr().String(), mux)
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveResult
	})

	body, err := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": "evt-slow-header", "event_type": "approval.instance.status_changed_v4",
			"app_id": "cli_test", "tenant_key": "tenant-test", "token": "verification-token",
		},
		"event": map[string]any{
			"approval_code": "approval-wallet-v1", "instance_code": "instance-slow-header", "status": "APPROVED",
		},
	})
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := connection.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		t.Fatalf("set connection deadline: %v", err)
	}
	started := time.Now()
	if _, err := fmt.Fprintf(connection,
		"POST /integrations/lark/events HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n",
		len(body),
	); err != nil {
		t.Fatalf("write partial headers: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := connection.Write(append([]byte("\r\n"), body...)); err != nil {
		t.Fatalf("complete request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("contended response status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("end-to-end acknowledgement took %s, want less than 3s", elapsed)
	}
}
