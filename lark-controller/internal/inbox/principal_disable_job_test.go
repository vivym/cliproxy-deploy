package inbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

func TestPrincipalDisableJobKeyRotationFailsClosedUntilJobIsTerminal(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	oldKeyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new old keyring: %v", err)
	}
	externalID := recordHeldPrincipalDisable(t, ctx, store, oldKeyring, "evt-key-rotation")
	if err := store.ValidatePrincipalDisableJobKeyIDs(ctx, oldKeyring.KeyIDs()); err != nil {
		t.Fatalf("validate original key: %v", err)
	}
	newKeyring, err := newapi.NewGrantKeyring(
		bytes.Repeat([]byte{0x24}, 32),
		bytes.Repeat([]byte{0x42}, 32),
	)
	if err != nil {
		t.Fatalf("new rotated keyring: %v", err)
	}
	if err := store.ValidatePrincipalDisableJobKeyIDs(ctx, newKeyring.KeyIDs()); err != nil {
		t.Fatalf("validate rotated keyring: %v", err)
	}
	retired, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatalf("new retired keyring: %v", err)
	}
	err = store.ValidatePrincipalDisableJobKeyIDs(ctx, retired.KeyIDs())
	if err == nil || strings.Contains(err.Error(), oldKeyring.PrimaryKeyID()) ||
		strings.Contains(err.Error(), retired.PrimaryKeyID()) {
		t.Fatalf("missing historical key validation error = %v", err)
	}

	if released, err := store.ReleaseHeldPrincipalDisableJobs(ctx); err != nil || released != 1 {
		t.Fatalf("release held principal disable: released=%d err=%v", released, err)
	}
	job, found, err := store.ClaimNextPrincipalDisableJob(ctx)
	if err != nil || !found || job.ExternalID != externalID {
		t.Fatalf("claim principal disable: found=%t job=%+v err=%v", found, job, err)
	}
	if err := store.DeadLetterPrincipalDisableJob(
		ctx,
		job,
		inbox.PrincipalDisableFailureInvalidSealedPayload,
	); err != nil {
		t.Fatalf("dead-letter principal disable: %v", err)
	}
	if err := store.ValidatePrincipalDisableJobKeyIDs(ctx, retired.KeyIDs()); err != nil {
		t.Fatalf("terminal principal disable blocked key retirement: %v", err)
	}
}

func recordHeldPrincipalDisable(
	t *testing.T,
	ctx context.Context,
	store *inbox.Store,
	keyring *newapi.GrantKeyring,
	eventID string,
) string {
	t.Helper()
	request, receipt, err := newapi.PlanContactEventPrincipalDisable(
		"tenant-test",
		"ou-resigned",
		eventID,
	)
	if err != nil {
		t.Fatalf("plan principal disable: %v", err)
	}
	sealed, err := keyring.SealPrincipalDisable(request)
	if err != nil {
		t.Fatalf("seal principal disable: %v", err)
	}
	payload, err := json.Marshal(map[string]string{"subject_sha256": receipt.SubjectSHA256})
	if err != nil {
		t.Fatalf("encode sanitized payload: %v", err)
	}
	if _, err := store.Record(ctx, inbox.Event{
		Key: "lark:v2:" + eventID, SchemaVersion: "2.0", EventID: eventID,
		EventType: "contact.user.deleted_v3", AppID: "cli_test", TenantKey: "tenant-test",
		PayloadJSON: string(payload),
		PrincipalDisableJob: &inbox.PrincipalDisableJobDraft{
			ExternalID: sealed.ExternalID, RequestSHA256: sealed.RequestSHA256,
			SubjectSHA256: receipt.SubjectSHA256, KeyID: sealed.KeyID,
			Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		},
	}); err != nil {
		t.Fatalf("record principal disable: %v", err)
	}
	return request.ExternalID
}
