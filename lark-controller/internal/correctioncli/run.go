package correctioncli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

type resolutionStore interface {
	Close() error
	GetApprovalReversal(context.Context, string) (inbox.ApprovalReversal, error)
	GetApprovalReversalCorrectionIntentForOriginal(
		context.Context,
		string,
	) (inbox.ApprovalReversalCorrectionIntent, bool, error)
	GetApprovalReversalResolutionForOriginal(
		context.Context,
		string,
	) (inbox.ApprovalReversalResolution, bool, error)
	GetPendingApprovalReversal(context.Context, string, string) (inbox.ApprovalReversal, error)
	ListPendingApprovalReversals(context.Context, int) ([]inbox.ApprovalReversal, error)
	ClaimApprovalReversalCorrectionIntent(
		context.Context,
		inbox.ApprovalReversalCorrectionIntent,
	) (inbox.ApprovalReversalCorrectionIntent, error)
	AbandonApprovalReversalCorrectionIntent(
		context.Context,
		inbox.ApprovalReversalCorrectionIntent,
		string,
	) error
	BlockApprovalReversalCorrectionIntent(
		context.Context,
		inbox.ApprovalReversalCorrectionIntent,
		string,
	) error
	ResolveApprovalReversal(
		context.Context,
		inbox.ApprovalReversalResolution,
	) (inbox.ApprovalReversalResolution, error)
}

type correctionClient interface {
	Preview(context.Context, newapi.Identity) (newapi.EntitlementCorrectionPreview, error)
	Correct(
		context.Context,
		newapi.EntitlementCorrectionRequest,
	) (newapi.EntitlementCorrectionResponse, error)
}

type dependencies struct {
	loadSecret        func(string) (string, error)
	openStore         func(string) (resolutionStore, error)
	openReadOnlyStore func(string) (resolutionStore, error)
	newClient         func(string, string, time.Duration) (correctionClient, error)
}

type optionalInt64 struct {
	value int64
	set   bool
}

func (value *optionalInt64) String() string {
	if !value.set {
		return ""
	}
	return strconv.FormatInt(value.value, 10)
}

func (value *optionalInt64) Set(raw string) error {
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return errors.New("value must be a signed 64-bit integer")
	}
	value.value = parsed
	value.set = true
	return nil
}

type options struct {
	newAPIBaseURL       string
	correctionSecret    string
	controllerDB        string
	reversalEventKey    string
	externalID          string
	originalExternalID  string
	policyVersion       string
	subject             string
	operator            string
	reason              string
	changeTicket        string
	levelCode           string
	walletDelta         optionalInt64
	expectedWalletQuota optionalInt64
	expectedVersion     optionalInt64
	apply               bool
	listPending         bool
	limit               int
	timeout             time.Duration
}

func parseOptions(arguments []string, errorOutput io.Writer) (options, error) {
	var parsed options
	flags := flag.NewFlagSet("lark-correction", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	flags.StringVar(&parsed.newAPIBaseURL, "new-api-base-url", "", "New API internal origin")
	flags.StringVar(&parsed.correctionSecret, "correction-secret-file", "", "correction credential file")
	flags.StringVar(&parsed.controllerDB, "controller-db", "", "Controller SQLite path")
	flags.StringVar(&parsed.reversalEventKey, "reversal-event-key", "", "pending reversal event key")
	flags.StringVar(&parsed.externalID, "external-id", "", "new correction external ID")
	flags.StringVar(&parsed.originalExternalID, "original-external-id", "", "original grant external ID")
	flags.StringVar(&parsed.policyVersion, "policy-version", "", "target policy version")
	flags.StringVar(&parsed.subject, "subject", "", "Lark integration subject")
	flags.StringVar(&parsed.operator, "operator", "", "operator identity")
	flags.StringVar(&parsed.reason, "reason", "", "correction reason")
	flags.StringVar(&parsed.changeTicket, "change-ticket", "", "change ticket")
	flags.Var(&parsed.walletDelta, "wallet-delta", "signed wallet quota delta")
	flags.Var(&parsed.expectedWalletQuota, "expected-wallet-quota", "expected wallet quota")
	flags.StringVar(&parsed.levelCode, "level-code", "", "absolute target subscription level")
	flags.Var(&parsed.expectedVersion, "expected-assignment-version", "expected assignment version")
	flags.BoolVar(&parsed.apply, "apply", false, "apply the correction after preview checks")
	flags.BoolVar(&parsed.listPending, "list-pending", false, "list pending approval reversals")
	flags.IntVar(&parsed.limit, "limit", 100, "pending reversal list limit")
	flags.DurationVar(&parsed.timeout, "timeout", 10*time.Second, "New API request timeout")
	if err := flags.Parse(arguments); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, errors.New("positional arguments are not accepted")
	}
	if parsed.listPending {
		if parsed.controllerDB == "" || parsed.limit < 1 || parsed.limit > 1000 {
			return options{}, errors.New("pending list requires controller DB and a limit between 1 and 1000")
		}
		if parsed.newAPIBaseURL != "" || parsed.correctionSecret != "" ||
			parsed.reversalEventKey != "" || parsed.externalID != "" || parsed.originalExternalID != "" ||
			parsed.policyVersion != "" || parsed.subject != "" || parsed.operator != "" ||
			parsed.reason != "" || parsed.changeTicket != "" || parsed.levelCode != "" ||
			parsed.walletDelta.set || parsed.expectedWalletQuota.set || parsed.expectedVersion.set || parsed.apply {
			return options{}, errors.New("pending list does not accept correction or apply flags")
		}
		return parsed, nil
	}
	if parsed.newAPIBaseURL == "" || parsed.correctionSecret == "" || parsed.controllerDB == "" ||
		parsed.reversalEventKey == "" || parsed.externalID == "" || parsed.originalExternalID == "" ||
		parsed.policyVersion == "" || parsed.subject == "" || parsed.operator == "" ||
		parsed.reason == "" || parsed.changeTicket == "" || parsed.timeout <= 0 {
		return options{}, errors.New("all common correction flags and a positive timeout are required")
	}
	walletMode := parsed.walletDelta.set || parsed.expectedWalletQuota.set
	subscriptionMode := parsed.levelCode != "" || parsed.expectedVersion.set
	if walletMode == subscriptionMode {
		return options{}, errors.New("select exactly one wallet or subscription correction")
	}
	if walletMode && (!parsed.walletDelta.set || !parsed.expectedWalletQuota.set) {
		return options{}, errors.New("wallet correction requires delta and expected wallet quota")
	}
	if subscriptionMode && (parsed.levelCode == "" || !parsed.expectedVersion.set) {
		return options{}, errors.New("subscription correction requires level and expected assignment version")
	}
	return parsed, nil
}

func buildCorrectionRequest(parsed options) newapi.EntitlementCorrectionRequest {
	request := newapi.EntitlementCorrectionRequest{
		ExternalID: parsed.externalID, Source: "correction", PolicyVersion: parsed.policyVersion,
		Identity: newapi.Identity{ProviderSlug: "lark", Subject: parsed.subject},
		Evidence: newapi.CorrectionEvidence{
			Operator: parsed.operator, Reason: parsed.reason, ChangeTicket: parsed.changeTicket,
			OriginalExternalID: parsed.originalExternalID,
		},
	}
	if parsed.walletDelta.set {
		expected := parsed.expectedWalletQuota.value
		request.Correction = newapi.Correction{
			Type: "wallet_quota", QuotaDelta: parsed.walletDelta.value,
			ExpectedWalletQuota: &expected,
		}
	} else {
		expected := parsed.expectedVersion.value
		request.Correction = newapi.Correction{
			Type: "subscription_level", LevelCode: parsed.levelCode,
			ExpectedAssignmentVersion: &expected,
		}
	}
	return request
}

func correctionSubjectSHA256(subject string) string {
	digest := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(digest[:])
}

func validatePreviewMatchesRequest(
	request newapi.EntitlementCorrectionRequest,
	preview newapi.EntitlementCorrectionPreview,
) error {
	switch request.Correction.Type {
	case "wallet_quota":
		if request.Correction.ExpectedWalletQuota == nil ||
			*request.Correction.ExpectedWalletQuota != preview.WalletQuota {
			return errors.New("expected wallet quota does not match current preview")
		}
	case "subscription_level":
		if preview.ManagedSubscription == nil ||
			request.Correction.ExpectedAssignmentVersion == nil ||
			*request.Correction.ExpectedAssignmentVersion != preview.ManagedSubscription.AssignmentVersion {
			return errors.New("expected assignment version does not match current preview")
		}
	default:
		return errors.New("unsupported correction type")
	}
	return nil
}

func writeOutput(output io.Writer, payload any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(payload)
}

func approvalCorrectionResult(result newapi.CorrectionResult) inbox.ApprovalCorrectionResult {
	return inbox.ApprovalCorrectionResult{
		CorrectionType: result.GrantType, QuotaDelta: result.QuotaDelta,
		WalletQuota: result.WalletQuota, LevelCode: result.LevelCode,
		SubscriptionID: result.SubscriptionID, AssignmentVersion: result.AssignmentVersion,
		Transition: result.Transition,
	}
}

func correctionResultFromApproval(result inbox.ApprovalCorrectionResult) newapi.CorrectionResult {
	return newapi.CorrectionResult{
		GrantType: result.CorrectionType, QuotaDelta: result.QuotaDelta,
		WalletQuota: result.WalletQuota, LevelCode: result.LevelCode,
		SubscriptionID: result.SubscriptionID, AssignmentVersion: result.AssignmentVersion,
		Transition: result.Transition,
	}
}

func writeAppliedOutput(
	output io.Writer,
	status string,
	externalID string,
	result newapi.CorrectionResult,
	resolvedAt time.Time,
	replayed bool,
) error {
	return writeOutput(output, struct {
		Mode       string                  `json:"mode"`
		Status     string                  `json:"status"`
		ExternalID string                  `json:"external_id"`
		Result     newapi.CorrectionResult `json:"result"`
		ResolvedAt time.Time               `json:"resolved_at"`
		Replayed   bool                    `json:"resolution_replayed"`
	}{
		Mode: "applied", Status: status, ExternalID: externalID,
		Result: result, ResolvedAt: resolvedAt, Replayed: replayed,
	})
}

func writeExistingResolutionOutput(
	output io.Writer,
	resolution inbox.ApprovalReversalResolution,
) error {
	return writeOutput(output, struct {
		Mode       string                  `json:"mode"`
		Status     string                  `json:"status"`
		ExternalID string                  `json:"external_id"`
		Result     newapi.CorrectionResult `json:"result"`
		ResolvedAt time.Time               `json:"resolved_at"`
	}{
		Mode: "existing_resolution", Status: resolution.ResponseStatus,
		ExternalID: resolution.CorrectionExternalID,
		Result:     correctionResultFromApproval(resolution.Result),
		ResolvedAt: resolution.ResolvedAt,
	})
}

func writeExistingIntentOutput(
	output io.Writer,
	intent inbox.ApprovalReversalCorrectionIntent,
) error {
	return writeOutput(output, struct {
		Mode string `json:"mode"`
		correctionIntentOutput
	}{
		Mode: "existing_intent", correctionIntentOutput: correctionIntentOutputFrom(intent),
	})
}

func correctionIntentMatches(
	stored inbox.ApprovalReversalCorrectionIntent,
	requested inbox.ApprovalReversalCorrectionIntent,
) bool {
	return stored.OriginalExternalID == requested.OriginalExternalID &&
		stored.OriginalSubjectSHA256 == requested.OriginalSubjectSHA256 &&
		stored.CorrectionExternalID == requested.CorrectionExternalID &&
		stored.CorrectionRequestSHA256 == requested.CorrectionRequestSHA256 &&
		stored.CorrectionType == requested.CorrectionType &&
		stored.Operator == requested.Operator && stored.Reason == requested.Reason &&
		stored.ChangeTicket == requested.ChangeTicket
}

func correctionIntentOperationError(
	operation string,
	operationErr error,
	finalizationErr error,
) error {
	if finalizationErr != nil {
		return fmt.Errorf(
			"%s: %w (finalize Controller correction intent: %v)",
			operation, operationErr, finalizationErr,
		)
	}
	return fmt.Errorf("%s: %w", operation, operationErr)
}

func abandonFreshCorrectionIntent(
	ctx context.Context,
	store resolutionStore,
	intent inbox.ApprovalReversalCorrectionIntent,
	intentWasPreexisting bool,
	failureCode string,
) error {
	if intentWasPreexisting {
		return nil
	}
	return store.AbandonApprovalReversalCorrectionIntent(ctx, intent, failureCode)
}

func finalizeCorrectionIntentForRemoteError(
	ctx context.Context,
	store resolutionStore,
	intent inbox.ApprovalReversalCorrectionIntent,
	err error,
) error {
	var apiError *newapi.APIError
	if !errors.As(err, &apiError) || apiError.Retryable {
		return nil
	}
	switch apiError.Code {
	case "external_id_payload_mismatch", "correction_already_applied":
		return store.BlockApprovalReversalCorrectionIntent(ctx, intent, apiError.Code)
	case "invalid_request",
		"integration_unauthorized",
		"principal_disabled",
		"correction_state_mismatch",
		"correction_original_grant_mismatch",
		"policy_version_mismatch",
		"unmanaged_subscription_conflict",
		"managed_plan_mismatch",
		"unknown_level",
		"quota_out_of_range":
		return store.AbandonApprovalReversalCorrectionIntent(ctx, intent, apiError.Code)
	default:
		return nil
	}
}

type correctionIntentOutput struct {
	ExternalID     string                                       `json:"external_id"`
	RequestSHA256  string                                       `json:"request_sha256"`
	CorrectionType string                                       `json:"correction_type"`
	Operator       string                                       `json:"operator"`
	Reason         string                                       `json:"reason"`
	ChangeTicket   string                                       `json:"change_ticket"`
	Status         inbox.ApprovalReversalCorrectionIntentStatus `json:"status"`
	FailureCode    string                                       `json:"failure_code,omitempty"`
	ClaimedAt      time.Time                                    `json:"claimed_at"`
}

func correctionIntentOutputFrom(
	intent inbox.ApprovalReversalCorrectionIntent,
) correctionIntentOutput {
	return correctionIntentOutput{
		ExternalID: intent.CorrectionExternalID, RequestSHA256: intent.CorrectionRequestSHA256,
		CorrectionType: intent.CorrectionType,
		Operator:       intent.Operator, Reason: intent.Reason, ChangeTicket: intent.ChangeTicket,
		Status: intent.Status, FailureCode: intent.FailureCode, ClaimedAt: intent.ClaimedAt,
	}
}

type pendingReversalOutput struct {
	EventKey              string                          `json:"event_key"`
	ApprovalCode          string                          `json:"approval_code"`
	TargetInstanceCode    string                          `json:"target_instance_code"`
	OriginalExternalID    string                          `json:"original_external_id"`
	OriginalGrantStatus   inbox.EntitlementGrantJobStatus `json:"original_grant_status"`
	OriginalGrantType     string                          `json:"original_grant_type"`
	OriginalQuotaDelta    int64                           `json:"original_quota_delta,omitempty"`
	OriginalPeriodQuota   int64                           `json:"original_period_quota,omitempty"`
	OriginalPolicyVersion string                          `json:"original_policy_version"`
	OriginalBusinessCode  string                          `json:"original_business_code"`
	Result                inbox.ApprovalReversalResult    `json:"result"`
	Reason                inbox.ApprovalReversalReason    `json:"reason"`
	CreatedAt             time.Time                       `json:"created_at"`
	CorrectionIntent      *correctionIntentOutput         `json:"correction_intent,omitempty"`
}

func pendingReversalOutputs(
	ctx context.Context,
	store resolutionStore,
	reversals []inbox.ApprovalReversal,
) ([]pendingReversalOutput, error) {
	output := make([]pendingReversalOutput, 0, len(reversals))
	for _, reversal := range reversals {
		item := pendingReversalOutputFrom(reversal)
		intent, found, err := store.GetApprovalReversalCorrectionIntentForOriginal(
			ctx, reversal.OriginalExternalID,
		)
		if err != nil {
			return nil, fmt.Errorf("load pending reversal correction intent: %w", err)
		}
		if found {
			summary := correctionIntentOutputFrom(intent)
			item.CorrectionIntent = &summary
		}
		output = append(output, item)
	}
	return output, nil
}

func pendingReversalOutputFrom(reversal inbox.ApprovalReversal) pendingReversalOutput {
	return pendingReversalOutput{
		EventKey: reversal.EventKey, ApprovalCode: reversal.ApprovalCode,
		TargetInstanceCode:    reversal.TargetInstanceCode,
		OriginalExternalID:    reversal.OriginalExternalID,
		OriginalGrantStatus:   reversal.OriginalGrantStatus,
		OriginalGrantType:     reversal.OriginalGrantType,
		OriginalQuotaDelta:    reversal.OriginalQuotaDelta,
		OriginalPeriodQuota:   reversal.OriginalPeriodQuota,
		OriginalPolicyVersion: reversal.OriginalPolicyVersion,
		OriginalBusinessCode:  reversal.OriginalBusinessCode,
		Result:                reversal.Result, Reason: reversal.Reason, CreatedAt: reversal.CreatedAt,
	}
}

func runWithDependencies(
	ctx context.Context,
	arguments []string,
	output io.Writer,
	errorOutput io.Writer,
	deps dependencies,
) error {
	parsed, err := parseOptions(arguments, errorOutput)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if parsed.listPending {
		store, err := deps.openReadOnlyStore(parsed.controllerDB)
		if err != nil {
			return fmt.Errorf("open Controller correction store: %w", err)
		}
		defer func() { _ = store.Close() }()
		reversals, err := store.ListPendingApprovalReversals(ctx, parsed.limit)
		if err != nil {
			return err
		}
		items, err := pendingReversalOutputs(ctx, store, reversals)
		if err != nil {
			return err
		}
		return writeOutput(output, struct {
			Mode      string                  `json:"mode"`
			Reversals []pendingReversalOutput `json:"reversals"`
		}{Mode: "list_pending", Reversals: items})
	}
	request := buildCorrectionRequest(parsed)
	requestSHA256, err := newapi.CorrectionRequestSHA256(request)
	if err != nil {
		return err
	}
	openStore := deps.openReadOnlyStore
	if parsed.apply {
		openStore = deps.openStore
	}
	store, err := openStore(parsed.controllerDB)
	if err != nil {
		return fmt.Errorf("open Controller correction store: %w", err)
	}
	defer func() { _ = store.Close() }()
	reversal, err := store.GetApprovalReversal(ctx, parsed.reversalEventKey)
	if err != nil {
		return fmt.Errorf("load approval reversal: %w", err)
	}
	if reversal.OriginalExternalID != parsed.originalExternalID {
		return inbox.ErrApprovalReversalResolutionMismatch
	}
	originalSubjectSHA256 := correctionSubjectSHA256(request.Identity.Subject)
	if reversal.OriginalSubjectSHA256 != originalSubjectSHA256 {
		return fmt.Errorf(
			"%w: subject does not match original grant",
			inbox.ErrApprovalReversalResolutionMismatch,
		)
	}
	if reversal.OriginalGrantType != request.Correction.Type {
		return fmt.Errorf(
			"%w: correction type %s does not match original grant type %s",
			inbox.ErrApprovalReversalResolutionMismatch,
			request.Correction.Type,
			reversal.OriginalGrantType,
		)
	}
	if reversal.Resolution != nil {
		resolution := reversal.Resolution
		if resolution.CorrectionExternalID != request.ExternalID ||
			resolution.CorrectionRequestSHA256 != requestSHA256 {
			return inbox.ErrApprovalReversalResolutionMismatch
		}
		if !parsed.apply {
			return writeExistingResolutionOutput(output, *resolution)
		}
		return writeAppliedOutput(
			output, resolution.ResponseStatus, resolution.CorrectionExternalID,
			correctionResultFromApproval(resolution.Result), resolution.ResolvedAt, true,
		)
	}
	if _, err := store.GetPendingApprovalReversal(
		ctx, parsed.reversalEventKey, parsed.originalExternalID,
	); err != nil {
		return fmt.Errorf("load pending approval reversal: %w", err)
	}
	existing, found, err := store.GetApprovalReversalResolutionForOriginal(
		ctx, parsed.originalExternalID,
	)
	if err != nil {
		return fmt.Errorf("load existing Controller correction receipt: %w", err)
	}
	if found {
		if existing.OriginalExternalID != parsed.originalExternalID ||
			existing.OriginalSubjectSHA256 != originalSubjectSHA256 ||
			existing.CorrectionExternalID != request.ExternalID ||
			existing.CorrectionRequestSHA256 != requestSHA256 {
			return inbox.ErrApprovalReversalResolutionMismatch
		}
		if !parsed.apply {
			return writeExistingResolutionOutput(output, existing)
		}
		existing.EventKey = parsed.reversalEventKey
		resolution, err := store.ResolveApprovalReversal(ctx, existing)
		if err != nil {
			return fmt.Errorf("attach Controller correction receipt for %s: %w", request.ExternalID, err)
		}
		return writeAppliedOutput(
			output, resolution.ResponseStatus, resolution.CorrectionExternalID,
			correctionResultFromApproval(resolution.Result), resolution.ResolvedAt, true,
		)
	}
	requestedIntent := inbox.ApprovalReversalCorrectionIntent{
		EventKey:                parsed.reversalEventKey,
		OriginalExternalID:      parsed.originalExternalID,
		OriginalSubjectSHA256:   originalSubjectSHA256,
		CorrectionExternalID:    request.ExternalID,
		CorrectionRequestSHA256: requestSHA256,
		CorrectionType:          request.Correction.Type,
		Operator:                parsed.operator,
		Reason:                  parsed.reason,
		ChangeTicket:            parsed.changeTicket,
	}
	existingIntent, intentFound, err := store.GetApprovalReversalCorrectionIntentForOriginal(
		ctx, parsed.originalExternalID,
	)
	if err != nil {
		return fmt.Errorf("load Controller correction intent: %w", err)
	}
	if intentFound {
		if !correctionIntentMatches(existingIntent, requestedIntent) {
			return fmt.Errorf(
				"%w: original grant is claimed by %s",
				inbox.ErrApprovalReversalResolutionMismatch,
				existingIntent.CorrectionExternalID,
			)
		}
		if existingIntent.Status != inbox.ApprovalReversalCorrectionIntentActive {
			return fmt.Errorf(
				"%w: correction intent %s is %s (%s)",
				inbox.ErrApprovalReversalResolutionMismatch,
				existingIntent.CorrectionExternalID,
				existingIntent.Status,
				existingIntent.FailureCode,
			)
		}
		if !parsed.apply {
			return writeExistingIntentOutput(output, existingIntent)
		}
	}
	// Preview mode has no durable intent to abandon. Apply mode replaces this
	// sentinel with the durable claim's replay state.
	intentWasPreexisting := !parsed.apply
	if parsed.apply {
		claimedIntent, err := store.ClaimApprovalReversalCorrectionIntent(ctx, requestedIntent)
		if err != nil {
			return fmt.Errorf("claim Controller correction intent for %s: %w", request.ExternalID, err)
		}
		if claimedIntent.Status != inbox.ApprovalReversalCorrectionIntentActive {
			return fmt.Errorf(
				"%w: correction intent %s is %s (%s)",
				inbox.ErrApprovalReversalResolutionMismatch,
				claimedIntent.CorrectionExternalID,
				claimedIntent.Status,
				claimedIntent.FailureCode,
			)
		}
		intentWasPreexisting = claimedIntent.Replayed
		requestedIntent = claimedIntent
	}
	secret, err := deps.loadSecret(parsed.correctionSecret)
	if err != nil {
		finalizationErr := abandonFreshCorrectionIntent(
			ctx, store, requestedIntent, intentWasPreexisting, "credential_load_failed",
		)
		return correctionIntentOperationError("load correction credential", err, finalizationErr)
	}
	client, err := deps.newClient(parsed.newAPIBaseURL, secret, parsed.timeout)
	if err != nil {
		finalizationErr := abandonFreshCorrectionIntent(
			ctx, store, requestedIntent, intentWasPreexisting, "client_initialization_failed",
		)
		return correctionIntentOperationError("initialize New API correction client", err, finalizationErr)
	}
	previewContext, cancelPreview := context.WithTimeout(ctx, parsed.timeout)
	preview, err := client.Preview(previewContext, request.Identity)
	cancelPreview()
	if err != nil {
		finalizationErr := abandonFreshCorrectionIntent(
			ctx, store, requestedIntent, intentWasPreexisting, "preview_failed",
		)
		return correctionIntentOperationError("preview New API correction", err, finalizationErr)
	}
	previewMatchErr := validatePreviewMatchesRequest(request, preview)
	expectedStateMatches := previewMatchErr == nil
	if !parsed.apply {
		return writeOutput(output, struct {
			Mode                 string                              `json:"mode"`
			ExpectedStateMatches bool                                `json:"expected_state_matches"`
			Reversal             pendingReversalOutput               `json:"reversal"`
			Current              newapi.EntitlementCorrectionPreview `json:"current"`
			Correction           newapi.Correction                   `json:"correction"`
		}{
			Mode: "preview", ExpectedStateMatches: expectedStateMatches,
			Reversal: pendingReversalOutputFrom(reversal), Current: preview, Correction: request.Correction,
		})
	}
	if previewMatchErr != nil && !intentWasPreexisting {
		finalizationErr := abandonFreshCorrectionIntent(
			ctx, store, requestedIntent, false, "preview_state_mismatch",
		)
		return correctionIntentOperationError(
			"validate fresh New API correction preview", previewMatchErr, finalizationErr,
		)
	}
	correctionContext, cancelCorrection := context.WithTimeout(ctx, parsed.timeout)
	response, err := client.Correct(correctionContext, request)
	cancelCorrection()
	if err != nil {
		finalizationErr := finalizeCorrectionIntentForRemoteError(
			ctx, store, requestedIntent, err,
		)
		return correctionIntentOperationError("apply New API correction", err, finalizationErr)
	}
	resolution, err := store.ResolveApprovalReversal(ctx, inbox.ApprovalReversalResolution{
		EventKey: parsed.reversalEventKey, OriginalExternalID: parsed.originalExternalID,
		OriginalSubjectSHA256: originalSubjectSHA256,
		CorrectionExternalID:  request.ExternalID, CorrectionRequestSHA256: requestSHA256,
		Operator: parsed.operator, Reason: parsed.reason, ChangeTicket: parsed.changeTicket,
		ResponseStatus: response.Status, Result: approvalCorrectionResult(response.Result),
	})
	if err != nil {
		return fmt.Errorf("record Controller correction resolution for %s: %w", request.ExternalID, err)
	}
	return writeAppliedOutput(
		output, response.Status, response.ExternalID, response.Result,
		resolution.ResolvedAt, resolution.Replayed,
	)
}

func Run(
	ctx context.Context,
	arguments []string,
	output io.Writer,
	errorOutput io.Writer,
	httpClient *http.Client,
) error {
	return runWithDependencies(ctx, arguments, output, errorOutput, dependencies{
		loadSecret: newapi.LoadCorrectionSecretFile,
		openStore: func(path string) (resolutionStore, error) {
			return inbox.OpenCorrection(path)
		},
		openReadOnlyStore: func(path string) (resolutionStore, error) {
			return inbox.OpenReadOnly(path)
		},
		newClient: func(baseURL, secret string, timeout time.Duration) (correctionClient, error) {
			client := httpClient
			if client == nil {
				client = &http.Client{Timeout: timeout}
			}
			return newapi.NewCorrectionClient(newapi.CorrectionConfig{
				BaseURL: baseURL, CorrectionSecret: secret, HTTPClient: client,
			})
		},
	})
}
