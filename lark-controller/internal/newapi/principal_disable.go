package newapi

import (
	"encoding/json"
	"errors"
	"time"
)

const contactUserDeletedEventType = "contact.user.deleted_v3"

type PrincipalDisableReceipt struct {
	ExternalID    string
	RequestSHA256 string
	SubjectSHA256 string
}

func PlanContactEventPrincipalDisable(
	tenantKey string,
	openID string,
	eventID string,
) (PrincipalDisableRequest, PrincipalDisableReceipt, error) {
	subject := tenantKey + ":" + openID
	request := PrincipalDisableRequest{
		ExternalID: "lark:disable:" + eventID,
		Source:     "contact_event",
		Identity: Identity{
			ProviderSlug: "lark",
			Subject:      subject,
		},
		Reason: contactUserDeletedEventType,
	}
	_, requestSHA256, err := canonicalizePrincipalDisableRequest(request)
	if err != nil || !validIdentifier(tenantKey, 128) || !validIdentifier(openID, 128) ||
		!validIdentifier(eventID, 200) {
		return PrincipalDisableRequest{}, PrincipalDisableReceipt{},
			errors.New("invalid contact event principal disable input")
	}
	return request, PrincipalDisableReceipt{
		ExternalID: request.ExternalID, RequestSHA256: requestSHA256,
		SubjectSHA256: sha256Hex([]byte(subject)),
	}, nil
}

func PlanEmploymentReconciliationPrincipalDisable(
	tenantKey string,
	openID string,
	evidenceDate string,
	employmentStatus string,
) (PrincipalDisableRequest, PrincipalDisableReceipt, error) {
	if _, err := time.Parse(time.DateOnly, evidenceDate); err != nil {
		return PrincipalDisableRequest{}, PrincipalDisableReceipt{},
			errors.New("invalid employment reconciliation evidence date")
	}
	reason := ""
	switch employmentStatus {
	case "resigned":
		reason = "lark_employment_resigned"
	case "exited":
		reason = "lark_employment_exited"
	case "not_found":
		reason = "lark_employment_not_found_confirmed"
	default:
		return PrincipalDisableRequest{}, PrincipalDisableReceipt{},
			errors.New("invalid employment reconciliation status")
	}
	subject := tenantKey + ":" + openID
	request := PrincipalDisableRequest{
		ExternalID: "lark:disable-reconcile:" + subject + ":" + evidenceDate,
		Source:     "employment_reconciliation",
		Identity: Identity{
			ProviderSlug: "lark",
			Subject:      subject,
		},
		Reason: reason,
	}
	_, requestSHA256, err := canonicalizePrincipalDisableRequest(request)
	if err != nil || !validIdentifier(tenantKey, 128) || !validIdentifier(openID, 128) {
		return PrincipalDisableRequest{}, PrincipalDisableReceipt{},
			errors.New("invalid employment reconciliation principal disable input")
	}
	return request, PrincipalDisableReceipt{
		ExternalID: request.ExternalID, RequestSHA256: requestSHA256,
		SubjectSHA256: sha256Hex([]byte(subject)),
	}, nil
}

func canonicalizePrincipalDisableRequest(
	request PrincipalDisableRequest,
) ([]byte, string, error) {
	if err := validatePrincipalDisableRequest(request); err != nil {
		return nil, "", err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, "", err
	}
	return payload, sha256Hex(payload), nil
}

func ValidatePrincipalDisableResult(
	status string,
	outcome string,
	principalVersion int64,
	authVersion int64,
) error {
	switch status {
	case "applied":
		if outcome == "disabled" && principalVersion > 0 && authVersion > 0 {
			return nil
		}
	case "noop":
		if outcome == "already_disabled" && principalVersion > 0 && authVersion == 0 {
			return nil
		}
		if outcome == "principal_absent" && principalVersion == 0 && authVersion == 0 {
			return nil
		}
	case "replayed":
		if outcome == "disabled" && principalVersion > 0 && authVersion > 0 {
			return nil
		}
		if outcome == "already_disabled" && principalVersion > 0 && authVersion == 0 {
			return nil
		}
		if outcome == "principal_absent" && principalVersion == 0 && authVersion == 0 {
			return nil
		}
	}
	return errors.New("invalid New API principal disable result")
}
