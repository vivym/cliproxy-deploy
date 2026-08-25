package tenantconfig

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

type Executor interface {
	Execute(context.Context, Change) (ExecutionResult, error)
}

type ExecutionResult struct {
	ResultDigest string
	Replayed     bool
}

type ApplyOptions struct {
	ChangeTicket string
	Executor     Executor
}

type ApplyStatus string

const (
	ApplyStatusSucceeded ApplyStatus = "succeeded"
	ApplyStatusFailed    ApplyStatus = "failed"
)

type OperationResult string

const (
	OperationResultApplied  OperationResult = "applied"
	OperationResultReplayed OperationResult = "replayed"
	OperationResultFailed   OperationResult = "failed"
)

type ApplyReceipt struct {
	FormatVersion  int                `json:"format_version"`
	PlanDigest     string             `json:"plan_digest"`
	CompiledDigest string             `json:"compiled_digest"`
	ChangeTicket   string             `json:"change_ticket"`
	Status         ApplyStatus        `json:"status"`
	Operations     []OperationReceipt `json:"operations"`
	Digest         string             `json:"digest"`
}

type OperationReceipt struct {
	ID           string          `json:"id"`
	Result       OperationResult `json:"result"`
	ResultDigest string          `json:"result_digest,omitempty"`
}

func Apply(
	ctx context.Context,
	plan ChangePlan,
	expectedDigest string,
	options ApplyOptions,
) (ApplyReceipt, error) {
	receipt := ApplyReceipt{
		FormatVersion: supportedFormatVersion, PlanDigest: plan.Digest,
		CompiledDigest: plan.CompiledDigest, ChangeTicket: options.ChangeTicket,
	}
	if err := validateApplyRequest(plan, expectedDigest, options); err != nil {
		return receipt, err
	}

	receipt.Operations = make([]OperationReceipt, 0, len(plan.Changes))
	for _, change := range plan.Changes {
		receiptOperationID := "sha256:" + sha256Hex([]byte(change.ID))
		result, err := options.Executor.Execute(ctx, change)
		if err != nil {
			receipt.Status = ApplyStatusFailed
			receipt.Operations = append(receipt.Operations, OperationReceipt{
				ID: receiptOperationID, Result: OperationResultFailed,
			})
			receipt.Digest = applyReceiptDigest(receipt)
			return receipt, errors.New("apply operation failed")
		}
		if !validResultDigest(result.ResultDigest) || result.ResultDigest != change.DesiredDigest {
			receipt.Status = ApplyStatusFailed
			receipt.Operations = append(receipt.Operations, OperationReceipt{
				ID: receiptOperationID, Result: OperationResultFailed,
			})
			receipt.Digest = applyReceiptDigest(receipt)
			return receipt, errors.New("apply operation result does not match the desired digest")
		}
		operationResult := OperationResultApplied
		if result.Replayed {
			operationResult = OperationResultReplayed
		}
		receipt.Operations = append(receipt.Operations, OperationReceipt{
			ID: receiptOperationID, Result: operationResult, ResultDigest: result.ResultDigest,
		})
	}
	receipt.Status = ApplyStatusSucceeded
	receipt.Digest = applyReceiptDigest(receipt)
	return receipt, nil
}

func validateApplyRequest(plan ChangePlan, expectedDigest string, options ApplyOptions) error {
	if plan.FormatVersion != supportedFormatVersion || plan.Digest == "" || plan.CompiledDigest == "" {
		return errors.New("valid format_version 2 change plan is required")
	}
	if expectedDigest == "" || expectedDigest != plan.Digest {
		return errors.New("expected plan digest does not match the supplied plan")
	}
	computedDigest, err := planDigest(plan)
	if err != nil {
		return err
	}
	if computedDigest != plan.Digest {
		return errors.New("change plan content does not match its digest")
	}
	if len(plan.Blockers) != 0 {
		return errors.New("change plan contains blockers")
	}
	if len(plan.ObservedTargets) != 3 || plan.ObservedTargets[0] != TargetLocal ||
		plan.ObservedTargets[1] != TargetNewAPI || plan.ObservedTargets[2] != TargetLark {
		return errors.New("change plan requires local, New API, and Lark observations")
	}
	if !validChangeTicket(options.ChangeTicket) {
		return errors.New("change ticket must be a non-empty printable identifier of at most 128 characters")
	}
	if options.Executor == nil {
		return errors.New("change executor is required")
	}
	previousID := ""
	previousSequence := ChangeSequence(-1)
	for _, change := range plan.Changes {
		if change.ID == "" || !change.ValidOperation() || change.Sequence < previousSequence ||
			(change.Sequence == previousSequence && change.ID <= previousID) ||
			change.Resource == "" || !validHexDigest(change.DesiredDigest) ||
			!validHexDigest(change.PayloadSHA256) ||
			sha256Hex(change.Payload) != change.PayloadSHA256 {
			return errors.New("change plan contains an invalid or tampered operation")
		}
		previousID = change.ID
		previousSequence = change.Sequence
	}
	return nil
}

func validChangeTicket(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func validResultDigest(value string) bool {
	if validHexDigest(value) {
		return true
	}
	return strings.HasPrefix(value, "sha256:") && validHexDigest(strings.TrimPrefix(value, "sha256:"))
}

func applyReceiptDigest(receipt ApplyReceipt) string {
	authority := struct {
		FormatVersion  int                `json:"format_version"`
		PlanDigest     string             `json:"plan_digest"`
		CompiledDigest string             `json:"compiled_digest"`
		ChangeTicket   string             `json:"change_ticket"`
		Status         ApplyStatus        `json:"status"`
		Operations     []OperationReceipt `json:"operations"`
	}{
		FormatVersion: receipt.FormatVersion, PlanDigest: receipt.PlanDigest,
		CompiledDigest: receipt.CompiledDigest, ChangeTicket: receipt.ChangeTicket,
		Status: receipt.Status, Operations: receipt.Operations,
	}
	contents, err := json.Marshal(authority)
	if err != nil {
		panic(err)
	}
	return "sha256:" + sha256Hex(contents)
}
