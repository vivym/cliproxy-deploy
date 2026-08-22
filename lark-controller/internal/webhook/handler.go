package webhook

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

const (
	defaultBodyLimit    int64 = 1 << 20
	defaultInboxTimeout       = 2 * time.Second
	larkACKDeadline           = 3 * time.Second
)

type Config struct {
	VerificationToken      string
	EncryptKey             string
	AppID                  string
	TenantKey              string
	BodyLimit              int64
	InboxTimeout           time.Duration
	PrincipalDisableSealer PrincipalDisableSealer
}

type Recorder interface {
	Record(context.Context, inbox.Event) (duplicate bool, err error)
}

type PrincipalDisableSealer interface {
	SealPrincipalDisable(newapi.PrincipalDisableRequest) (newapi.SealedPrincipalDisableRequest, error)
}

type Handler struct {
	config   Config
	recorder Recorder
}

func NewHandler(config Config, recorder Recorder) (*Handler, error) {
	if config.VerificationToken == "" || config.AppID == "" || config.TenantKey == "" {
		return nil, errors.New("verification token, app id, and tenant key are required")
	}
	if recorder == nil {
		return nil, errors.New("event recorder is required")
	}
	if isNilPrincipalDisableSealer(config.PrincipalDisableSealer) {
		return nil, errors.New("principal disable payload sealer is required")
	}
	if config.BodyLimit == 0 {
		config.BodyLimit = defaultBodyLimit
	}
	if config.BodyLimit < 1 {
		return nil, errors.New("body limit must be positive")
	}
	if config.InboxTimeout == 0 {
		config.InboxTimeout = defaultInboxTimeout
	}
	if config.InboxTimeout < 0 || config.InboxTimeout >= larkACKDeadline {
		return nil, errors.New("inbox timeout must be positive and less than 3 seconds")
	}
	return &Handler{config: config, recorder: recorder}, nil
}

func isNilPrincipalDisableSealer(sealer PrincipalDisableSealer) bool {
	if sealer == nil {
		return true
	}
	value := reflect.ValueOf(sealer)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(response, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	inboxContext, cancel := context.WithTimeout(request.Context(), h.config.InboxTimeout)
	defer cancel()

	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, h.config.BodyLimit))
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request")
		return
	}
	plainBody, err := h.verifyAndDecrypt(request, body)
	if err != nil {
		writeError(response, http.StatusUnauthorized, "verification_failed")
		return
	}
	var envelope eventEnvelope
	if err := json.Unmarshal(plainBody, &envelope); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json")
		return
	}
	if envelope.Type == "url_verification" {
		if envelope.Schema != "" && envelope.Schema != "2.0" {
			writeError(response, http.StatusBadRequest, "invalid_event")
			return
		}
		h.handleChallenge(response, envelope)
		return
	}
	if err := h.recordEvent(inboxContext, envelope); err != nil {
		if errors.Is(err, errVerificationFailed) {
			writeError(response, http.StatusUnauthorized, "verification_failed")
			return
		}
		if errors.Is(err, inbox.ErrEventPayloadMismatch) {
			writeError(response, http.StatusConflict, "event_id_payload_mismatch")
			return
		}
		if errors.Is(err, errInboxUnavailable) {
			writeError(response, http.StatusServiceUnavailable, "inbox_unavailable")
			return
		}
		writeError(response, http.StatusBadRequest, "invalid_event")
		return
	}

	writeJSON(response, http.StatusOK, map[string]string{"msg": "success"})
}

var errVerificationFailed = errors.New("event verification failed")
var errInboxUnavailable = errors.New("event inbox unavailable")

type eventEnvelope struct {
	Schema    string          `json:"schema"`
	UUID      string          `json:"uuid"`
	Challenge string          `json:"challenge"`
	Token     string          `json:"token"`
	Type      string          `json:"type"`
	Header    eventHeader     `json:"header"`
	Event     json.RawMessage `json:"event"`
}

type eventHeader struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	AppID     string `json:"app_id"`
	TenantKey string `json:"tenant_key"`
	Token     string `json:"token"`
}

type approvalEventPayload struct {
	ApprovalCode         string `json:"approval_code,omitempty"`
	InstanceCode         string `json:"instance_code,omitempty"`
	Status               string `json:"status,omitempty"`
	OperateTime          string `json:"operate_time,omitempty"`
	RevertedInstanceCode string `json:"reverted_instance_code,omitempty"`
}

type contactUserDeletedPayload struct {
	Object struct {
		OpenID string `json:"open_id"`
	} `json:"object"`
}

func (h *Handler) verifyAndDecrypt(request *http.Request, body []byte) ([]byte, error) {
	if h.config.EncryptKey == "" {
		return body, nil
	}
	timestamp := request.Header.Get(larkevent.EventRequestTimestamp)
	nonce := request.Header.Get(larkevent.EventRequestNonce)
	signature := request.Header.Get(larkevent.EventSignature)
	expected := larkevent.Signature(timestamp, nonce, h.config.EncryptKey, string(body))
	if timestamp == "" || nonce == "" || signature == "" || !secretEqual(signature, expected) {
		return nil, errVerificationFailed
	}
	var encrypted larkevent.EventEncryptMsg
	if err := json.Unmarshal(body, &encrypted); err != nil || encrypted.Encrypt == "" {
		return nil, errVerificationFailed
	}
	plainBody, err := larkevent.EventDecrypt(encrypted.Encrypt, h.config.EncryptKey)
	if err != nil {
		return nil, errVerificationFailed
	}
	return plainBody, nil
}

func (h *Handler) handleChallenge(response http.ResponseWriter, envelope eventEnvelope) {
	token := envelope.Token
	if token == "" {
		token = envelope.Header.Token
	}
	if envelope.Challenge == "" || !secretEqual(token, h.config.VerificationToken) {
		writeError(response, http.StatusUnauthorized, "verification_failed")
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"challenge": envelope.Challenge})
}

func (h *Handler) recordEvent(ctx context.Context, envelope eventEnvelope) error {
	switch envelope.Schema {
	case "2.0":
		return h.recordV2Event(ctx, envelope)
	case "":
		return h.recordV1Event(ctx, envelope)
	default:
		return errors.New("unsupported event schema")
	}
}

func (h *Handler) recordV2Event(ctx context.Context, envelope eventEnvelope) error {
	if envelope.Schema != "2.0" || envelope.Header.EventID == "" || envelope.Header.EventType == "" {
		return errors.New("incomplete v2 event header")
	}
	if !secretEqual(envelope.Header.Token, h.config.VerificationToken) ||
		!secretEqual(envelope.Header.AppID, h.config.AppID) ||
		!secretEqual(envelope.Header.TenantKey, h.config.TenantKey) {
		return errVerificationFailed
	}
	if len(envelope.Event) == 0 || string(envelope.Event) == "null" {
		return errors.New("missing v2 event body")
	}
	if envelope.Header.EventType == "contact.user.deleted_v3" {
		return h.recordContactUserDeleted(ctx, envelope)
	}
	var approval approvalEventPayload
	if err := json.Unmarshal(envelope.Event, &approval); err != nil {
		return err
	}
	normalized, err := json.Marshal(approval)
	if err != nil {
		return err
	}
	_, err = h.recorder.Record(ctx, inbox.Event{
		Key:           "lark:v2:" + envelope.Header.EventID,
		SchemaVersion: envelope.Schema,
		EventID:       envelope.Header.EventID,
		EventType:     envelope.Header.EventType,
		AppID:         envelope.Header.AppID,
		TenantKey:     envelope.Header.TenantKey,
		ApprovalCode:  approval.ApprovalCode,
		InstanceCode:  approval.InstanceCode,
		Status:        approval.Status,
		PayloadJSON:   string(normalized),
	})
	if err != nil {
		if errors.Is(err, inbox.ErrEventPayloadMismatch) {
			return err
		}
		return fmt.Errorf("%w: %v", errInboxUnavailable, err)
	}
	return nil
}

func (h *Handler) recordContactUserDeleted(ctx context.Context, envelope eventEnvelope) error {
	var deleted contactUserDeletedPayload
	if err := json.Unmarshal(envelope.Event, &deleted); err != nil || deleted.Object.OpenID == "" {
		return errors.New("invalid contact user deleted event")
	}
	request, receipt, err := newapi.PlanContactEventPrincipalDisable(
		envelope.Header.TenantKey,
		deleted.Object.OpenID,
		envelope.Header.EventID,
	)
	if err != nil {
		return err
	}
	sealed, err := h.config.PrincipalDisableSealer.SealPrincipalDisable(request)
	if err != nil {
		return fmt.Errorf("%w: seal principal disable request", errInboxUnavailable)
	}
	normalized, err := json.Marshal(struct {
		SubjectSHA256 string `json:"subject_sha256"`
	}{SubjectSHA256: receipt.SubjectSHA256})
	if err != nil {
		return err
	}
	_, err = h.recorder.Record(ctx, inbox.Event{
		Key:           "lark:v2:" + envelope.Header.EventID,
		SchemaVersion: envelope.Schema,
		EventID:       envelope.Header.EventID,
		EventType:     envelope.Header.EventType,
		AppID:         envelope.Header.AppID,
		TenantKey:     envelope.Header.TenantKey,
		PayloadJSON:   string(normalized),
		PrincipalDisableJob: &inbox.PrincipalDisableJobDraft{
			ExternalID: sealed.ExternalID, RequestSHA256: sealed.RequestSHA256,
			SubjectSHA256: receipt.SubjectSHA256, KeyID: sealed.KeyID,
			Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		},
	})
	if err != nil {
		if errors.Is(err, inbox.ErrEventPayloadMismatch) {
			return err
		}
		return fmt.Errorf("%w: %v", errInboxUnavailable, err)
	}
	return nil
}

func (h *Handler) recordV1Event(ctx context.Context, envelope eventEnvelope) error {
	if envelope.Type != "event_callback" || envelope.UUID == "" ||
		len(envelope.Event) == 0 || string(envelope.Event) == "null" {
		return errors.New("incomplete v1 event")
	}
	if !secretEqual(envelope.Token, h.config.VerificationToken) {
		return errVerificationFailed
	}
	var event struct {
		Type      string `json:"type"`
		AppID     string `json:"app_id"`
		TenantKey string `json:"tenant_key"`
		approvalEventPayload
	}
	if err := json.Unmarshal(envelope.Event, &event); err != nil {
		return err
	}
	if event.Type == "" || !secretEqual(event.AppID, h.config.AppID) ||
		!secretEqual(event.TenantKey, h.config.TenantKey) {
		return errVerificationFailed
	}
	normalized, err := json.Marshal(event.approvalEventPayload)
	if err != nil {
		return err
	}
	_, err = h.recorder.Record(ctx, inbox.Event{
		Key:           "lark:v1:" + envelope.UUID,
		SchemaVersion: "1.0",
		EventID:       envelope.UUID,
		EventType:     event.Type,
		AppID:         event.AppID,
		TenantKey:     event.TenantKey,
		ApprovalCode:  event.ApprovalCode,
		InstanceCode:  event.InstanceCode,
		Status:        event.Status,
		PayloadJSON:   string(normalized),
	})
	if err != nil {
		if errors.Is(err, inbox.ErrEventPayloadMismatch) {
			return err
		}
		return fmt.Errorf("%w: %v", errInboxUnavailable, err)
	}
	return nil
}

func secretEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func writeError(response http.ResponseWriter, status int, code string) {
	writeJSON(response, status, map[string]string{"error": code})
}

func writeJSON(response http.ResponseWriter, status int, payload any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(payload)
}
