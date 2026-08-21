package inbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	oauthCredentialBytes       = 32
	oauthAuthorizationStateTTL = 5 * time.Minute
	oauthLoginCodeTTL          = time.Minute
	oauthAccessHandleTTL       = time.Minute
	maxOAuthStateBytes         = 1024
	maxOAuthRedirectURIBytes   = 2048
	maxOAuthSubjectBytes       = 255
	maxOAuthDisplayNameRunes   = 20
)

var ErrOAuthCredentialInvalid = errors.New("OAuth credential is invalid, expired, or already consumed")

type OAuthAuthorizationState struct {
	NewAPIState string
	RedirectURI string
}

type OAuthIdentity struct {
	Subject  string
	Username string
	Name     string
}

func (s *Store) CreateOAuthAuthorizationState(
	ctx context.Context,
	state OAuthAuthorizationState,
) (string, error) {
	if s == nil || s.database == nil || !validOAuthAuthorizationState(state) {
		return "", errors.New("valid OAuth authorization state is required")
	}
	raw, digest, err := generateOAuthCredential()
	if err != nil {
		return "", err
	}
	now := s.currentTime()
	if _, err := s.database.ExecContext(ctx, `
INSERT INTO oauth_states (
    state_hash, new_api_state, redirect_uri, expires_at, consumed_at, created_at
) VALUES (?, ?, ?, ?, '', ?)`,
		digest[:],
		state.NewAPIState,
		state.RedirectURI,
		now.Add(oauthAuthorizationStateTTL).UnixNano(),
		now.Format(time.RFC3339Nano),
	); err != nil {
		return "", fmt.Errorf("store OAuth authorization state: %w", err)
	}
	return raw, nil
}

func (s *Store) ConsumeOAuthAuthorizationState(
	ctx context.Context,
	raw string,
) (OAuthAuthorizationState, error) {
	digest, valid := hashOAuthCredential(raw)
	if s == nil || s.database == nil || !valid {
		return OAuthAuthorizationState{}, ErrOAuthCredentialInvalid
	}
	now := s.currentTime()
	var state OAuthAuthorizationState
	err := s.database.QueryRowContext(ctx, `
UPDATE oauth_states
SET consumed_at = ?
WHERE state_hash = ? AND consumed_at = '' AND expires_at > ?
RETURNING new_api_state, redirect_uri`,
		now.Format(time.RFC3339Nano),
		digest[:],
		now.UnixNano(),
	).Scan(&state.NewAPIState, &state.RedirectURI)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthAuthorizationState{}, ErrOAuthCredentialInvalid
	}
	if err != nil {
		return OAuthAuthorizationState{}, fmt.Errorf("consume OAuth authorization state: %w", err)
	}
	return state, nil
}

func (s *Store) CreateOAuthLoginCode(ctx context.Context, identity OAuthIdentity) (string, error) {
	if s == nil || s.database == nil || !validOAuthIdentity(identity) {
		return "", errors.New("valid OAuth identity is required")
	}
	raw, digest, err := generateOAuthCredential()
	if err != nil {
		return "", err
	}
	now := s.currentTime()
	if _, err := s.database.ExecContext(ctx, `
INSERT INTO oauth_login_codes (
    code_hash, subject, username, display_name, expires_at, consumed_at, created_at
) VALUES (?, ?, ?, ?, ?, '', ?)`,
		digest[:],
		identity.Subject,
		identity.Username,
		identity.Name,
		now.Add(oauthLoginCodeTTL).UnixNano(),
		now.Format(time.RFC3339Nano),
	); err != nil {
		return "", fmt.Errorf("store OAuth login code: %w", err)
	}
	return raw, nil
}

func (s *Store) ExchangeOAuthLoginCode(ctx context.Context, raw string) (string, error) {
	loginDigest, valid := hashOAuthCredential(raw)
	if s == nil || s.database == nil || !valid {
		return "", ErrOAuthCredentialInvalid
	}
	accessHandle, accessDigest, err := generateOAuthCredential()
	if err != nil {
		return "", err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin OAuth login code exchange: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.currentTime()
	formattedNow := now.Format(time.RFC3339Nano)
	var identity OAuthIdentity
	err = tx.QueryRowContext(ctx, `
UPDATE oauth_login_codes
SET consumed_at = ?
WHERE code_hash = ? AND consumed_at = '' AND expires_at > ?
	RETURNING subject, username, display_name`,
		formattedNow,
		loginDigest[:],
		now.UnixNano(),
	).Scan(&identity.Subject, &identity.Username, &identity.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrOAuthCredentialInvalid
	}
	if err != nil {
		return "", fmt.Errorf("consume OAuth login code: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO oauth_access_handles (
    handle_hash, subject, username, display_name, expires_at, consumed_at, created_at
) VALUES (?, ?, ?, ?, ?, '', ?)`,
		accessDigest[:],
		identity.Subject,
		identity.Username,
		identity.Name,
		now.Add(oauthAccessHandleTTL).UnixNano(),
		formattedNow,
	); err != nil {
		return "", fmt.Errorf("store OAuth access handle: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit OAuth login code exchange: %w", err)
	}
	return accessHandle, nil
}

func (s *Store) ConsumeOAuthAccessHandle(ctx context.Context, raw string) (OAuthIdentity, error) {
	digest, valid := hashOAuthCredential(raw)
	if s == nil || s.database == nil || !valid {
		return OAuthIdentity{}, ErrOAuthCredentialInvalid
	}
	now := s.currentTime()
	var identity OAuthIdentity
	err := s.database.QueryRowContext(ctx, `
UPDATE oauth_access_handles
SET consumed_at = ?
WHERE handle_hash = ? AND consumed_at = '' AND expires_at > ?
RETURNING subject, username, display_name`,
		now.Format(time.RFC3339Nano),
		digest[:],
		now.UnixNano(),
	).Scan(&identity.Subject, &identity.Username, &identity.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthIdentity{}, ErrOAuthCredentialInvalid
	}
	if err != nil {
		return OAuthIdentity{}, fmt.Errorf("consume OAuth access handle: %w", err)
	}
	return identity, nil
}

func (s *Store) currentTime() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func generateOAuthCredential() (string, [sha256.Size]byte, error) {
	random := make([]byte, oauthCredentialBytes)
	defer func() {
		for index := range random {
			random[index] = 0
		}
	}()
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		return "", [sha256.Size]byte{}, errors.New("generate OAuth credential")
	}
	raw := base64.RawURLEncoding.EncodeToString(random)
	return raw, sha256.Sum256([]byte(raw)), nil
}

func hashOAuthCredential(raw string) ([sha256.Size]byte, bool) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(decoded) != oauthCredentialBytes {
		return [sha256.Size]byte{}, false
	}
	for index := range decoded {
		decoded[index] = 0
	}
	return sha256.Sum256([]byte(raw)), true
}

func validOAuthAuthorizationState(state OAuthAuthorizationState) bool {
	return state.NewAPIState != "" && len(state.NewAPIState) <= maxOAuthStateBytes &&
		state.RedirectURI != "" && len(state.RedirectURI) <= maxOAuthRedirectURIBytes &&
		!strings.ContainsAny(state.NewAPIState, "\r\n") &&
		!strings.ContainsAny(state.RedirectURI, "\r\n")
}

func validOAuthIdentity(identity OAuthIdentity) bool {
	wantUsername, err := OAuthUsername(identity.Subject)
	if err != nil || identity.Username != wantUsername ||
		identity.Name == "" || !utf8.ValidString(identity.Name) ||
		utf8.RuneCountInString(identity.Name) > maxOAuthDisplayNameRunes {
		return false
	}
	return !strings.ContainsAny(identity.Name, "\r\n")
}

func OAuthUsername(subject string) (string, error) {
	if !validOAuthSubject(subject) {
		return "", errors.New("valid Lark OAuth subject is required")
	}
	digest := sha256.Sum256([]byte(subject))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])
	return "lark_" + strings.ToLower(encoded[:15]), nil
}

func validOAuthSubject(subject string) bool {
	if subject == "" || len(subject) > maxOAuthSubjectBytes || strings.Count(subject, ":") != 1 {
		return false
	}
	tenantKey, openID, _ := strings.Cut(subject, ":")
	return validLarkIdentifier(tenantKey) && validLarkIdentifier(openID)
}

func validLarkIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '_' || character == '-') {
			return false
		}
	}
	return true
}
