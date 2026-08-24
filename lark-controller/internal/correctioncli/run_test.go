package correctioncli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

type fakeResolutionStore struct {
	reversal        inbox.ApprovalReversal
	pending         []inbox.ApprovalReversal
	intent          *inbox.ApprovalReversalCorrectionIntent
	intentHistory   []inbox.ApprovalReversalCorrectionIntent
	originalReceipt *inbox.ApprovalReversalResolution
	resolved        *inbox.ApprovalReversalResolution
}

func TestRunHelpReturnsSuccess(t *testing.T) {
	var output bytes.Buffer
	var errorOutput bytes.Buffer

	if err := Run(context.Background(), []string{"--help"}, &output, &errorOutput, nil); err != nil {
		t.Fatalf("help returned an error: %v", err)
	}
	if !strings.Contains(errorOutput.String(), "Usage of lark-correction:") {
		t.Fatalf("help output = %q", errorOutput.String())
	}
}

func (store *fakeResolutionStore) ListPendingApprovalReversals(
	context.Context,
	int,
) ([]inbox.ApprovalReversal, error) {
	return store.pending, nil
}

func (store *fakeResolutionStore) Close() error { return nil }

func (store *fakeResolutionStore) GetApprovalReversal(
	context.Context,
	string,
) (inbox.ApprovalReversal, error) {
	return store.reversal, nil
}

func (store *fakeResolutionStore) GetApprovalReversalResolutionForOriginal(
	context.Context,
	string,
) (inbox.ApprovalReversalResolution, bool, error) {
	if store.originalReceipt == nil {
		return inbox.ApprovalReversalResolution{}, false, nil
	}
	return *store.originalReceipt, true, nil
}

func (store *fakeResolutionStore) GetApprovalReversalCorrectionIntentForOriginal(
	context.Context,
	string,
) (inbox.ApprovalReversalCorrectionIntent, bool, error) {
	if store.intent == nil {
		return inbox.ApprovalReversalCorrectionIntent{}, false, nil
	}
	return *store.intent, true, nil
}

func (store *fakeResolutionStore) GetPendingApprovalReversal(
	context.Context,
	string,
	string,
) (inbox.ApprovalReversal, error) {
	return store.reversal, nil
}

func (store *fakeResolutionStore) ClaimApprovalReversalCorrectionIntent(
	_ context.Context,
	intent inbox.ApprovalReversalCorrectionIntent,
) (inbox.ApprovalReversalCorrectionIntent, error) {
	if store.intent != nil {
		if !correctionIntentMatches(*store.intent, intent) {
			return inbox.ApprovalReversalCorrectionIntent{}, inbox.ErrApprovalReversalResolutionMismatch
		}
		if store.intent.Status == inbox.ApprovalReversalCorrectionIntentAbandoned {
			intent.Status = inbox.ApprovalReversalCorrectionIntentActive
			intent.ClaimedAt = time.Date(2026, 8, 23, 8, 56, 0, 0, time.UTC)
			store.intent = &intent
			return intent, nil
		}
		replayed := *store.intent
		replayed.Replayed = true
		return replayed, nil
	}
	intent.Status = inbox.ApprovalReversalCorrectionIntentActive
	intent.ClaimedAt = time.Date(2026, 8, 23, 8, 55, 0, 0, time.UTC)
	store.intent = &intent
	return intent, nil
}

func (store *fakeResolutionStore) AbandonApprovalReversalCorrectionIntent(
	_ context.Context,
	intent inbox.ApprovalReversalCorrectionIntent,
	failureCode string,
) error {
	return store.finalizeIntent(intent, inbox.ApprovalReversalCorrectionIntentAbandoned, failureCode)
}

func (store *fakeResolutionStore) BlockApprovalReversalCorrectionIntent(
	_ context.Context,
	intent inbox.ApprovalReversalCorrectionIntent,
	failureCode string,
) error {
	return store.finalizeIntent(intent, inbox.ApprovalReversalCorrectionIntentRemoteConflict, failureCode)
}

func (store *fakeResolutionStore) finalizeIntent(
	intent inbox.ApprovalReversalCorrectionIntent,
	status inbox.ApprovalReversalCorrectionIntentStatus,
	failureCode string,
) error {
	if store.intent == nil || !correctionIntentMatches(*store.intent, intent) ||
		store.intent.Status != inbox.ApprovalReversalCorrectionIntentActive {
		return inbox.ErrApprovalReversalResolutionMismatch
	}
	ended := *store.intent
	ended.Status = status
	ended.FailureCode = failureCode
	ended.EndedAt = time.Date(2026, 8, 23, 8, 57, 0, 0, time.UTC)
	store.intentHistory = append(store.intentHistory, ended)
	if status == inbox.ApprovalReversalCorrectionIntentAbandoned {
		store.intent = nil
	} else {
		store.intent = &ended
	}
	return nil
}

func (store *fakeResolutionStore) ResolveApprovalReversal(
	_ context.Context,
	resolution inbox.ApprovalReversalResolution,
) (inbox.ApprovalReversalResolution, error) {
	store.resolved = &resolution
	resolution.ResolvedAt = time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	if store.intent != nil {
		resolved := *store.intent
		resolved.Status = inbox.ApprovalReversalCorrectionIntentResolved
		resolved.EndedAt = resolution.ResolvedAt
		store.intent = &resolved
	}
	return resolution, nil
}

type fakeCorrectionClient struct {
	preview    newapi.EntitlementCorrectionPreview
	previewErr error
	corrected  *newapi.EntitlementCorrectionRequest
	correction newapi.EntitlementCorrectionResponse
	correctErr error
}

func (client *fakeCorrectionClient) Preview(
	context.Context,
	newapi.Identity,
) (newapi.EntitlementCorrectionPreview, error) {
	return client.preview, client.previewErr
}

func (client *fakeCorrectionClient) Correct(
	_ context.Context,
	request newapi.EntitlementCorrectionRequest,
) (newapi.EntitlementCorrectionResponse, error) {
	client.corrected = &request
	return client.correction, client.correctErr
}

func correctionCLIArguments(apply bool) []string {
	arguments := []string{
		"--new-api-base-url", "http://new-api:3001",
		"--correction-secret-file", "/run/secrets/lark_correction_secret",
		"--controller-db", "/var/lib/lark-controller/controller.sqlite",
		"--reversal-event-key", "lark:v2:reversal-event",
		"--external-id", "lark:correction:CHG-2026-0060:wallet",
		"--original-external-id", "lark:wallet-topup:instance-original",
		"--policy-version", "employee-v1",
		"--subject", "tenant-key:ou_1",
		"--operator", "ops@example.com",
		"--reason", "reverted approval after partial use",
		"--change-ticket", "CHG-2026-0060",
		"--wallet-delta", "-1000000",
		"--expected-wallet-quota", "3000000",
	}
	if apply {
		arguments = append(arguments, "--apply")
	}
	return arguments
}

func replaceCorrectionCLIArgument(arguments []string, name, value string) []string {
	replaced := append([]string(nil), arguments...)
	for index := 0; index+1 < len(replaced); index++ {
		if replaced[index] == name {
			replaced[index+1] = value
			return replaced
		}
	}
	panic("correction CLI argument not found: " + name)
}

func correctionCLIDependencies(
	store *fakeResolutionStore,
	client *fakeCorrectionClient,
) dependencies {
	return dependencies{
		loadSecret:        func(string) (string, error) { return strings.Repeat("s", 32), nil },
		openStore:         func(string) (resolutionStore, error) { return store, nil },
		openReadOnlyStore: func(string) (resolutionStore, error) { return store, nil },
		newClient:         func(string, string, time.Duration) (correctionClient, error) { return client, nil },
	}
}

func int64Pointer(value int64) *int64 { return &value }

func TestRunPreviewsWithoutApplyingCorrection(t *testing.T) {
	store := &fakeResolutionStore{reversal: inbox.ApprovalReversal{
		EventKey: "lark:v2:reversal-event", OriginalExternalID: "lark:wallet-topup:instance-original",
		OriginalSubjectSHA256: correctionSubjectSHA256("tenant-key:ou_1"),
		OriginalGrantType:     "wallet_quota", OriginalQuotaDelta: 2_500_000,
	}}
	client := &fakeCorrectionClient{preview: newapi.EntitlementCorrectionPreview{
		WalletQuota: 3_000_000, UsedQuota: 7_000_000, LastLoginAt: 1_788_192_100,
	}}
	dependencies := correctionCLIDependencies(store, client)
	dependencies.openStore = func(string) (resolutionStore, error) {
		return nil, errors.New("writable Store must not be opened while previewing")
	}
	var output bytes.Buffer
	if err := runWithDependencies(
		context.Background(), correctionCLIArguments(false), &output, io.Discard,
		dependencies,
	); err != nil {
		t.Fatalf("preview correction: %v", err)
	}
	if client.corrected != nil || store.resolved != nil {
		t.Fatal("preview mode applied a correction")
	}
	if !strings.Contains(output.String(), `"mode":"preview"`) ||
		!strings.Contains(output.String(), `"wallet_quota":3000000`) ||
		!strings.Contains(output.String(), `"original_grant_type":"wallet_quota"`) ||
		strings.Contains(output.String(), `"OriginalGrantType"`) ||
		strings.Contains(output.String(), `"AuthorityApprovalCode"`) {
		t.Fatalf("preview output = %s", output.String())
	}
}

func TestRunRejectsCorrectionTypeDifferentFromOriginalGrant(t *testing.T) {
	store := &fakeResolutionStore{reversal: inbox.ApprovalReversal{
		EventKey: "lark:v2:reversal-event", OriginalExternalID: "lark:wallet-topup:instance-original",
		OriginalSubjectSHA256: correctionSubjectSHA256("tenant-key:ou_1"),
		OriginalGrantType:     "subscription_level", OriginalMonthlyQuota: 2_500_000,
	}}
	client := &fakeCorrectionClient{}
	err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), io.Discard, io.Discard,
		correctionCLIDependencies(store, client),
	)
	if !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) {
		t.Fatalf("mismatched correction type error = %v", err)
	}
	if client.corrected != nil || store.resolved != nil {
		t.Fatal("mismatched correction type reached a write path")
	}
}

func TestRunListsPendingReversalsWithoutCorrectionCredential(t *testing.T) {
	subjectSHA256 := correctionSubjectSHA256("tenant-key:ou_1")
	store := &fakeResolutionStore{
		pending: []inbox.ApprovalReversal{{
			EventKey: "lark:v2:reversal-event", ApprovalCode: "approval-wallet-v1",
			OriginalExternalID: "lark:wallet-topup:instance-original",
			OriginalGrantType:  "wallet_quota", OriginalQuotaDelta: 2_500_000,
			OriginalPolicyVersion: "employee-v1", OriginalBusinessCode: "topup_5",
			Reason: inbox.ApprovalReversalReasonManualReviewRequired,
		}},
		intent: &inbox.ApprovalReversalCorrectionIntent{
			OriginalExternalID:      "lark:wallet-topup:instance-original",
			OriginalSubjectSHA256:   subjectSHA256,
			CorrectionExternalID:    "lark:correction:CHG-2026-0060:wallet",
			CorrectionRequestSHA256: strings.Repeat("a", 64), CorrectionType: "wallet_quota",
			Operator: "ops@example.com", Reason: "reviewing reversal", ChangeTicket: "CHG-2026-0060",
			Status:    inbox.ApprovalReversalCorrectionIntentActive,
			ClaimedAt: time.Date(2026, 8, 23, 8, 55, 0, 0, time.UTC),
		},
	}
	client := &fakeCorrectionClient{}
	dependencies := correctionCLIDependencies(store, client)
	dependencies.openStore = func(string) (resolutionStore, error) {
		return nil, errors.New("writable Store must not be opened while listing")
	}
	var output bytes.Buffer
	if err := runWithDependencies(
		context.Background(), []string{
			"--list-pending", "--controller-db", "/var/lib/lark-controller/controller.sqlite",
		}, &output, io.Discard, dependencies,
	); err != nil {
		t.Fatalf("list pending corrections: %v", err)
	}
	if !strings.Contains(output.String(), `"mode":"list_pending"`) ||
		!strings.Contains(output.String(), `"event_key":"lark:v2:reversal-event"`) ||
		!strings.Contains(output.String(), `"correction_intent":{"external_id":"lark:correction:CHG-2026-0060:wallet"`) ||
		strings.Contains(output.String(), subjectSHA256) ||
		strings.Contains(output.String(), "original_subject_sha256") {
		t.Fatalf("pending correction output = %s", output.String())
	}
}

func TestRunAppliesAndResolvesCorrection(t *testing.T) {
	store := &fakeResolutionStore{reversal: inbox.ApprovalReversal{
		EventKey: "lark:v2:reversal-event", OriginalExternalID: "lark:wallet-topup:instance-original",
		OriginalSubjectSHA256: correctionSubjectSHA256("tenant-key:ou_1"),
		OriginalGrantType:     "wallet_quota", OriginalQuotaDelta: 2_500_000,
	}}
	finalQuota := int64(2_000_000)
	client := &fakeCorrectionClient{
		preview: newapi.EntitlementCorrectionPreview{WalletQuota: 3_000_000},
		correction: newapi.EntitlementCorrectionResponse{
			Status: "applied", ExternalID: "lark:correction:CHG-2026-0060:wallet", UserID: 42,
			Result: newapi.CorrectionResult{
				GrantType: "wallet_quota", QuotaDelta: -1_000_000, WalletQuota: &finalQuota,
			},
		},
	}
	var output bytes.Buffer
	if err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), &output, io.Discard,
		correctionCLIDependencies(store, client),
	); err != nil {
		t.Fatalf("apply correction: %v", err)
	}
	if client.corrected == nil || store.resolved == nil {
		t.Fatal("apply mode did not correct and resolve")
	}
	if store.resolved.CorrectionExternalID != client.corrected.ExternalID ||
		store.resolved.OriginalSubjectSHA256 != correctionSubjectSHA256("tenant-key:ou_1") ||
		store.resolved.CorrectionRequestSHA256 == "" ||
		store.resolved.Result.WalletQuota == nil ||
		*store.resolved.Result.WalletQuota != finalQuota {
		t.Fatalf("stored resolution = %+v", store.resolved)
	}
	if !strings.Contains(output.String(), `"mode":"applied"`) ||
		strings.Contains(output.String(), `"user_id"`) {
		t.Fatalf("apply output = %s", output.String())
	}
}

func TestRunLetsServerResolveStalePreviewForPreexistingIntentReplay(t *testing.T) {
	parsed, err := parseOptions(correctionCLIArguments(true), io.Discard)
	if err != nil {
		t.Fatalf("parse correction arguments: %v", err)
	}
	request := buildCorrectionRequest(parsed)
	requestSHA256, err := newapi.CorrectionRequestSHA256(request)
	if err != nil {
		t.Fatalf("hash correction request: %v", err)
	}
	store := &fakeResolutionStore{reversal: inbox.ApprovalReversal{
		EventKey: "lark:v2:reversal-event", OriginalExternalID: "lark:wallet-topup:instance-original",
		OriginalSubjectSHA256: correctionSubjectSHA256("tenant-key:ou_1"),
		OriginalGrantType:     "wallet_quota", OriginalQuotaDelta: 2_500_000,
	}, intent: &inbox.ApprovalReversalCorrectionIntent{
		EventKey: parsed.reversalEventKey, OriginalExternalID: parsed.originalExternalID,
		OriginalSubjectSHA256: correctionSubjectSHA256(parsed.subject),
		CorrectionExternalID:  request.ExternalID, CorrectionRequestSHA256: requestSHA256,
		CorrectionType: request.Correction.Type,
		Operator:       parsed.operator, Reason: parsed.reason, ChangeTicket: parsed.changeTicket,
		Status:    inbox.ApprovalReversalCorrectionIntentActive,
		ClaimedAt: time.Date(2026, 8, 23, 8, 55, 0, 0, time.UTC),
	}}
	finalQuota := int64(2_000_000)
	client := &fakeCorrectionClient{
		preview: newapi.EntitlementCorrectionPreview{WalletQuota: 2_000_000},
		correction: newapi.EntitlementCorrectionResponse{
			Status: "replayed", ExternalID: "lark:correction:CHG-2026-0060:wallet",
			Result: newapi.CorrectionResult{
				GrantType: "wallet_quota", QuotaDelta: -1_000_000, WalletQuota: &finalQuota,
			},
		},
	}
	err = runWithDependencies(
		context.Background(), correctionCLIArguments(true), io.Discard, io.Discard,
		correctionCLIDependencies(store, client),
	)
	if err != nil {
		t.Fatalf("replay after stale preview: %v", err)
	}
	if client.corrected == nil || store.resolved == nil {
		t.Fatal("stale preview prevented idempotent server replay")
	}
}

func TestRunAbandonsFreshIntentWhenPreviewStateIsStale(t *testing.T) {
	store := &fakeResolutionStore{reversal: inbox.ApprovalReversal{
		EventKey: "lark:v2:reversal-event", OriginalExternalID: "lark:wallet-topup:instance-original",
		OriginalSubjectSHA256: correctionSubjectSHA256("tenant-key:ou_1"),
		OriginalGrantType:     "wallet_quota", OriginalQuotaDelta: 2_500_000,
	}}
	client := &fakeCorrectionClient{
		preview: newapi.EntitlementCorrectionPreview{WalletQuota: 2_000_000},
	}
	err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), io.Discard, io.Discard,
		correctionCLIDependencies(store, client),
	)
	if err == nil || !strings.Contains(err.Error(), "expected wallet quota") {
		t.Fatalf("fresh stale preview error = %v", err)
	}
	if client.corrected != nil || store.intent != nil || len(store.intentHistory) != 1 ||
		store.intentHistory[0].Status != inbox.ApprovalReversalCorrectionIntentAbandoned ||
		store.intentHistory[0].FailureCode != "preview_state_mismatch" {
		t.Fatalf(
			"fresh stale preview state: corrected=%+v intent=%+v history=%+v",
			client.corrected, store.intent, store.intentHistory,
		)
	}
}

func TestRunReplaysAlreadyStoredControllerResolution(t *testing.T) {
	parsed, err := parseOptions(correctionCLIArguments(true), io.Discard)
	if err != nil {
		t.Fatalf("parse correction arguments: %v", err)
	}
	request := buildCorrectionRequest(parsed)
	requestSHA256, err := newapi.CorrectionRequestSHA256(request)
	if err != nil {
		t.Fatalf("hash correction request: %v", err)
	}
	finalQuota := int64(2_000_000)
	resolvedAt := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	store := &fakeResolutionStore{reversal: inbox.ApprovalReversal{
		EventKey: "lark:v2:reversal-event", OriginalExternalID: request.Evidence.OriginalExternalID,
		OriginalSubjectSHA256: correctionSubjectSHA256(request.Identity.Subject),
		OriginalGrantType:     "wallet_quota",
		Resolution: &inbox.ApprovalReversalResolution{
			CorrectionExternalID: request.ExternalID, CorrectionRequestSHA256: requestSHA256,
			ResponseStatus: "applied", ResolvedAt: resolvedAt,
			Result: inbox.ApprovalCorrectionResult{
				CorrectionType: "wallet_quota", QuotaDelta: -1_000_000, WalletQuota: &finalQuota,
			},
		},
	}}
	client := &fakeCorrectionClient{}
	var output bytes.Buffer
	if err := runWithDependencies(
		context.Background(), correctionCLIArguments(false), &output, io.Discard,
		correctionCLIDependencies(store, client),
	); err != nil {
		t.Fatalf("preview stored resolution: %v", err)
	}
	if !strings.Contains(output.String(), `"mode":"existing_resolution"`) ||
		strings.Contains(output.String(), `"resolution_replayed"`) {
		t.Fatalf("stored resolution preview output = %s", output.String())
	}
	output.Reset()
	if err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), &output, io.Discard,
		correctionCLIDependencies(store, client),
	); err != nil {
		t.Fatalf("replay stored resolution: %v", err)
	}
	if client.corrected != nil || store.resolved != nil {
		t.Fatal("stored resolution replay reached a write path")
	}
	if !strings.Contains(output.String(), `"resolution_replayed":true`) {
		t.Fatalf("stored resolution output = %s", output.String())
	}
}

func TestRunPersistsIntentAcrossWinnerResponseLossAndRejectsDifferentCommand(t *testing.T) {
	store := &fakeResolutionStore{reversal: inbox.ApprovalReversal{
		EventKey: "lark:v2:reversal-event", OriginalExternalID: "lark:wallet-topup:instance-original",
		OriginalSubjectSHA256: correctionSubjectSHA256("tenant-key:ou_1"),
		OriginalGrantType:     "wallet_quota", OriginalQuotaDelta: 2_500_000,
	}}
	responseLost := errors.New("response lost after New API commit")
	winnerClient := &fakeCorrectionClient{
		preview:    newapi.EntitlementCorrectionPreview{WalletQuota: 3_000_000},
		correctErr: responseLost,
	}
	err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), io.Discard, io.Discard,
		correctionCLIDependencies(store, winnerClient),
	)
	if !errors.Is(err, responseLost) || store.intent == nil || store.resolved != nil {
		t.Fatalf("response-loss state: intent=%+v resolution=%+v err=%v", store.intent, store.resolved, err)
	}
	winnerExternalID := store.intent.CorrectionExternalID
	winnerRequestSHA256 := store.intent.CorrectionRequestSHA256

	loserArguments := replaceCorrectionCLIArgument(
		correctionCLIArguments(true), "--external-id", "lark:correction:CHG-2026-0061:wallet",
	)
	loserArguments = replaceCorrectionCLIArgument(loserArguments, "--change-ticket", "CHG-2026-0061")
	loserClient := &fakeCorrectionClient{}
	loserDependencies := correctionCLIDependencies(store, loserClient)
	loserDependencies.loadSecret = func(string) (string, error) {
		return "", errors.New("losing command must not read correction credential")
	}
	err = runWithDependencies(
		context.Background(), loserArguments, io.Discard, io.Discard, loserDependencies,
	)
	if !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) || loserClient.corrected != nil {
		t.Fatalf("different command crossed intent fence: corrected=%+v err=%v", loserClient.corrected, err)
	}
	if store.intent.CorrectionExternalID != winnerExternalID ||
		store.intent.CorrectionRequestSHA256 != winnerRequestSHA256 {
		t.Fatalf("losing command changed durable intent: %+v", store.intent)
	}

	var previewOutput bytes.Buffer
	if err := runWithDependencies(
		context.Background(), correctionCLIArguments(false), &previewOutput, io.Discard,
		loserDependencies,
	); err != nil {
		t.Fatalf("preview existing winner intent: %v", err)
	}
	if !strings.Contains(previewOutput.String(), `"mode":"existing_intent"`) ||
		!strings.Contains(previewOutput.String(), winnerExternalID) ||
		!strings.Contains(previewOutput.String(), winnerRequestSHA256) {
		t.Fatalf("existing intent preview output = %s", previewOutput.String())
	}

	finalQuota := int64(2_000_000)
	replayClient := &fakeCorrectionClient{
		preview: newapi.EntitlementCorrectionPreview{WalletQuota: 2_000_000},
		correction: newapi.EntitlementCorrectionResponse{
			Status: "replayed", ExternalID: winnerExternalID,
			Result: newapi.CorrectionResult{
				GrantType: "wallet_quota", QuotaDelta: -1_000_000, WalletQuota: &finalQuota,
			},
		},
	}
	if err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), io.Discard, io.Discard,
		correctionCLIDependencies(store, replayClient),
	); err != nil {
		t.Fatalf("recover winner from durable intent: %v", err)
	}
	if replayClient.corrected == nil || store.resolved == nil ||
		store.resolved.CorrectionExternalID != winnerExternalID {
		t.Fatalf("winner recovery: corrected=%+v resolution=%+v", replayClient.corrected, store.resolved)
	}
}

func TestRunAbandonsFreshIntentAfterDeterministicRemoteRejection(t *testing.T) {
	store := &fakeResolutionStore{reversal: inbox.ApprovalReversal{
		EventKey: "lark:v2:reversal-event", OriginalExternalID: "lark:wallet-topup:instance-original",
		OriginalSubjectSHA256: correctionSubjectSHA256("tenant-key:ou_1"),
		OriginalGrantType:     "wallet_quota", OriginalQuotaDelta: 2_500_000,
	}}
	client := &fakeCorrectionClient{
		preview: newapi.EntitlementCorrectionPreview{WalletQuota: 3_000_000},
		correctErr: &newapi.APIError{
			StatusCode: http.StatusConflict, Code: "correction_state_mismatch", Retryable: false,
		},
	}
	err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), io.Discard, io.Discard,
		correctionCLIDependencies(store, client),
	)
	if err == nil || store.intent != nil || len(store.intentHistory) != 1 ||
		store.intentHistory[0].Status != inbox.ApprovalReversalCorrectionIntentAbandoned ||
		store.intentHistory[0].FailureCode != "correction_state_mismatch" {
		t.Fatalf(
			"deterministic rejection state: intent=%+v history=%+v err=%v",
			store.intent, store.intentHistory, err,
		)
	}

	replacementArguments := replaceCorrectionCLIArgument(
		correctionCLIArguments(true), "--external-id", "lark:correction:CHG-2026-0061:wallet",
	)
	replacementArguments = replaceCorrectionCLIArgument(
		replacementArguments, "--change-ticket", "CHG-2026-0061",
	)
	replacementClient := &fakeCorrectionClient{
		preview: newapi.EntitlementCorrectionPreview{WalletQuota: 3_000_000},
		correction: newapi.EntitlementCorrectionResponse{
			Status: "noop", ExternalID: "lark:correction:CHG-2026-0061:wallet",
			Result: newapi.CorrectionResult{
				GrantType: "wallet_quota", WalletQuota: int64Pointer(3_000_000),
			},
		},
	}
	if err := runWithDependencies(
		context.Background(), replacementArguments, io.Discard, io.Discard,
		correctionCLIDependencies(store, replacementClient),
	); err != nil {
		t.Fatalf("apply replacement after abandoned intent: %v", err)
	}
	if replacementClient.corrected == nil || store.resolved == nil {
		t.Fatalf(
			"replacement did not apply: corrected=%+v resolution=%+v",
			replacementClient.corrected, store.resolved,
		)
	}
}

func TestRunKeepsIntentActiveAfterUncertainRemoteFailure(t *testing.T) {
	store := &fakeResolutionStore{reversal: inbox.ApprovalReversal{
		EventKey: "lark:v2:reversal-event", OriginalExternalID: "lark:wallet-topup:instance-original",
		OriginalSubjectSHA256: correctionSubjectSHA256("tenant-key:ou_1"),
		OriginalGrantType:     "wallet_quota", OriginalQuotaDelta: 2_500_000,
	}}
	client := &fakeCorrectionClient{
		preview:    newapi.EntitlementCorrectionPreview{WalletQuota: 3_000_000},
		correctErr: &newapi.RequestError{Reason: "timeout", Retryable: true},
	}
	err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), io.Discard, io.Discard,
		correctionCLIDependencies(store, client),
	)
	if err == nil || store.intent == nil ||
		store.intent.Status != inbox.ApprovalReversalCorrectionIntentActive ||
		len(store.intentHistory) != 0 {
		t.Fatalf(
			"uncertain failure state: intent=%+v history=%+v err=%v",
			store.intent, store.intentHistory, err,
		)
	}
}

func TestRunBlocksReplacementAfterRemoteConflict(t *testing.T) {
	store := &fakeResolutionStore{reversal: inbox.ApprovalReversal{
		EventKey: "lark:v2:reversal-event", OriginalExternalID: "lark:wallet-topup:instance-original",
		OriginalSubjectSHA256: correctionSubjectSHA256("tenant-key:ou_1"),
		OriginalGrantType:     "wallet_quota", OriginalQuotaDelta: 2_500_000,
	}}
	client := &fakeCorrectionClient{
		preview: newapi.EntitlementCorrectionPreview{WalletQuota: 3_000_000},
		correctErr: &newapi.APIError{
			StatusCode: http.StatusConflict, Code: "correction_already_applied", Retryable: false,
		},
	}
	err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), io.Discard, io.Discard,
		correctionCLIDependencies(store, client),
	)
	if err == nil || store.intent == nil ||
		store.intent.Status != inbox.ApprovalReversalCorrectionIntentRemoteConflict ||
		store.intent.FailureCode != "correction_already_applied" {
		t.Fatalf("remote conflict state: intent=%+v err=%v", store.intent, err)
	}

	replacementArguments := replaceCorrectionCLIArgument(
		correctionCLIArguments(true), "--external-id", "lark:correction:CHG-2026-0061:wallet",
	)
	replacementArguments = replaceCorrectionCLIArgument(
		replacementArguments, "--change-ticket", "CHG-2026-0061",
	)
	replacementDeps := correctionCLIDependencies(store, &fakeCorrectionClient{})
	replacementDeps.loadSecret = func(string) (string, error) {
		return "", errors.New("remote conflict replacement must not read correction credential")
	}
	err = runWithDependencies(
		context.Background(), replacementArguments, io.Discard, io.Discard, replacementDeps,
	)
	if !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) {
		t.Fatalf("remote conflict replacement error = %v", err)
	}
}

func TestRunRejectsSubjectDifferentFromOriginalGrantBeforeCredentialOrNewAPI(t *testing.T) {
	store := &fakeResolutionStore{reversal: inbox.ApprovalReversal{
		EventKey: "lark:v2:reversal-event", OriginalExternalID: "lark:wallet-topup:instance-original",
		OriginalSubjectSHA256: correctionSubjectSHA256("tenant-key:ou_other"),
		OriginalGrantType:     "wallet_quota",
	}}
	client := &fakeCorrectionClient{}
	dependencies := correctionCLIDependencies(store, client)
	dependencies.loadSecret = func(string) (string, error) {
		return "", errors.New("correction credential must not be read")
	}
	err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), io.Discard, io.Discard,
		dependencies,
	)
	if !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) {
		t.Fatalf("mismatched correction subject error = %v", err)
	}
	if client.corrected != nil || store.resolved != nil {
		t.Fatal("mismatched correction subject reached a write path")
	}
}

func TestRunAttachesLateReversalToExistingReceiptWithoutCallingNewAPI(t *testing.T) {
	parsed, err := parseOptions(correctionCLIArguments(true), io.Discard)
	if err != nil {
		t.Fatalf("parse correction arguments: %v", err)
	}
	request := buildCorrectionRequest(parsed)
	requestSHA256, err := newapi.CorrectionRequestSHA256(request)
	if err != nil {
		t.Fatalf("hash correction request: %v", err)
	}
	finalQuota := int64(2_000_000)
	store := &fakeResolutionStore{
		reversal: inbox.ApprovalReversal{
			EventKey: parsed.reversalEventKey, OriginalExternalID: parsed.originalExternalID,
			OriginalSubjectSHA256: correctionSubjectSHA256(parsed.subject),
			OriginalGrantType:     "wallet_quota",
		},
		originalReceipt: &inbox.ApprovalReversalResolution{
			OriginalExternalID:    parsed.originalExternalID,
			OriginalSubjectSHA256: correctionSubjectSHA256(parsed.subject),
			CorrectionExternalID:  request.ExternalID, CorrectionRequestSHA256: requestSHA256,
			Operator: parsed.operator, Reason: parsed.reason, ChangeTicket: parsed.changeTicket,
			ResponseStatus: "applied", ResolvedAt: time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC),
			Result: inbox.ApprovalCorrectionResult{
				CorrectionType: "wallet_quota", QuotaDelta: -1_000_000, WalletQuota: &finalQuota,
			},
		},
	}
	deps := correctionCLIDependencies(store, &fakeCorrectionClient{})
	deps.loadSecret = func(string) (string, error) {
		return "", errors.New("existing receipt must bypass correction credential")
	}
	deps.newClient = func(string, string, time.Duration) (correctionClient, error) {
		return nil, errors.New("existing receipt must bypass New API")
	}
	var output bytes.Buffer
	if err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), &output, io.Discard, deps,
	); err != nil {
		t.Fatalf("attach late reversal receipt: %v", err)
	}
	if store.resolved == nil {
		t.Fatal("late receipt was not attached")
	}
	if !strings.Contains(output.String(), `"resolution_replayed":true`) {
		t.Fatalf("late receipt output = %s", output.String())
	}
}

func TestRunPreviewsExistingReceiptWithoutAttachingOrCallingNewAPI(t *testing.T) {
	parsed, err := parseOptions(correctionCLIArguments(false), io.Discard)
	if err != nil {
		t.Fatalf("parse correction arguments: %v", err)
	}
	request := buildCorrectionRequest(parsed)
	requestSHA256, err := newapi.CorrectionRequestSHA256(request)
	if err != nil {
		t.Fatalf("hash correction request: %v", err)
	}
	finalQuota := int64(2_000_000)
	store := &fakeResolutionStore{
		reversal: inbox.ApprovalReversal{
			EventKey: parsed.reversalEventKey, OriginalExternalID: parsed.originalExternalID,
			OriginalSubjectSHA256: correctionSubjectSHA256(parsed.subject),
			OriginalGrantType:     "wallet_quota",
		},
		originalReceipt: &inbox.ApprovalReversalResolution{
			OriginalExternalID:    parsed.originalExternalID,
			OriginalSubjectSHA256: correctionSubjectSHA256(parsed.subject),
			CorrectionExternalID:  request.ExternalID, CorrectionRequestSHA256: requestSHA256,
			ResponseStatus: "applied", ResolvedAt: time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC),
			Result: inbox.ApprovalCorrectionResult{
				CorrectionType: "wallet_quota", QuotaDelta: -1_000_000, WalletQuota: &finalQuota,
			},
		},
	}
	deps := correctionCLIDependencies(store, &fakeCorrectionClient{})
	deps.loadSecret = func(string) (string, error) {
		return "", errors.New("existing receipt preview must bypass correction credential")
	}
	var output bytes.Buffer
	if err := runWithDependencies(
		context.Background(), correctionCLIArguments(false), &output, io.Discard, deps,
	); err != nil {
		t.Fatalf("preview existing receipt: %v", err)
	}
	if store.resolved != nil || !strings.Contains(output.String(), `"mode":"existing_resolution"`) {
		t.Fatalf("existing receipt preview output=%s resolved=%+v", output.String(), store.resolved)
	}
}

func TestRunRejectsDifferentCorrectionForExistingOriginalBeforeCredentialOrNewAPI(t *testing.T) {
	store := &fakeResolutionStore{
		reversal: inbox.ApprovalReversal{
			EventKey:              "lark:v2:reversal-event",
			OriginalExternalID:    "lark:wallet-topup:instance-original",
			OriginalSubjectSHA256: correctionSubjectSHA256("tenant-key:ou_1"),
			OriginalGrantType:     "wallet_quota",
		},
		originalReceipt: &inbox.ApprovalReversalResolution{
			OriginalExternalID:      "lark:wallet-topup:instance-original",
			OriginalSubjectSHA256:   correctionSubjectSHA256("tenant-key:ou_1"),
			CorrectionExternalID:    "lark:correction:CHG-OTHER:wallet",
			CorrectionRequestSHA256: strings.Repeat("a", 64),
		},
	}
	deps := correctionCLIDependencies(store, &fakeCorrectionClient{})
	deps.loadSecret = func(string) (string, error) {
		return "", errors.New("mismatched receipt must bypass correction credential")
	}
	err := runWithDependencies(
		context.Background(), correctionCLIArguments(true), io.Discard, io.Discard, deps,
	)
	if !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) {
		t.Fatalf("different existing correction error = %v", err)
	}
	if store.resolved != nil {
		t.Fatalf("different existing correction stored resolution = %+v", store.resolved)
	}
}
