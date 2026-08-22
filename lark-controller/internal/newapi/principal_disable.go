package newapi

import (
	"encoding/json"
	"errors"
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
