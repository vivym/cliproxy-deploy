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
	oauthCredentialBytes             = 32
	oauthAuthorizationStateTTL       = 5 * time.Minute
	oauthLoginCodeTTL                = time.Minute
	oauthAccessHandleTTL             = time.Minute
	oauthCredentialPruneInterval     = time.Minute
	maxOAuthStateBytes               = 1024
	maxOAuthRedirectURIBytes         = 2048
	maxOAuthSubjectBytes             = 255
	maxOAuthDisplayNameRunes         = 20
	maxOutstandingOAuthStates        = 10_000
	maxOutstandingOAuthLoginCodes    = 5_000
	maxOutstandingOAuthAccessHandles = 5_000
)

var (
	ErrOAuthCredentialInvalid  = errors.New("OAuth credential is invalid, expired, or already consumed")
	ErrOAuthCredentialCapacity = errors.New("OAuth credential capacity is unavailable")
)

type oauthCredentialKind int

const (
	oauthCredentialState oauthCredentialKind = iota
	oauthCredentialLoginCode
	oauthCredentialAccessHandle
	oauthCredentialKindCount
)

var oauthCredentialTables = [oauthCredentialKindCount]string{
	"oauth_states",
	"oauth_login_codes",
	"oauth_access_handles",
}

var oauthCredentialLimits = [oauthCredentialKindCount]int64{
	maxOutstandingOAuthStates,
	maxOutstandingOAuthLoginCodes,
	maxOutstandingOAuthAccessHandles,
}

type OAuthAuthorizationState struct {
	NewAPIState string
	RedirectURI string
}

func NewOAuthAuthorizationState(newAPIState, redirectURI string) (OAuthAuthorizationState, error) {
	state := OAuthAuthorizationState{NewAPIState: newAPIState, RedirectURI: redirectURI}
	if !validOAuthAuthorizationState(state) {
		return OAuthAuthorizationState{}, errors.New("valid OAuth authorization state is required")
	}
	return state, nil
}

type OAuthIdentity struct {
	Subject  string
	Username string
	Name     string
}

func NewOAuthIdentity(subject, name string) (OAuthIdentity, error) {
	username, err := OAuthUsername(subject)
	if err != nil || name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "\r\n") {
		return OAuthIdentity{}, errors.New("valid Lark OAuth identity is required")
	}
	displayName := []rune(name)
	if len(displayName) > maxOAuthDisplayNameRunes {
		displayName = displayName[:maxOAuthDisplayNameRunes]
	}
	identity := OAuthIdentity{Subject: subject, Username: username, Name: string(displayName)}
	if !validOAuthIdentity(identity) {
		return OAuthIdentity{}, errors.New("valid Lark OAuth identity is required")
	}
	return identity, nil
}

func (s *Store) CreateOAuthAuthorizationState(
	ctx context.Context,
	state OAuthAuthorizationState,
) (string, error) {
	if s == nil || s.database == nil || !validOAuthAuthorizationState(state) {
		return "", errors.New("valid OAuth authorization state is required")
	}
	now := s.currentTime()
	if err := s.reserveOAuthCredential(ctx, oauthCredentialState, now); err != nil {
		return "", err
	}
	reserved := true
	defer func() {
		if reserved {
			s.releaseOAuthCredential(oauthCredentialState)
		}
	}()
	raw, digest, err := generateOAuthCredential()
	if err != nil {
		return "", err
	}
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
	reserved = false
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
DELETE FROM oauth_states
WHERE state_hash = ? AND consumed_at = '' AND expires_at > ?
RETURNING new_api_state, redirect_uri`,
		digest[:],
		now.UnixNano(),
	).Scan(&state.NewAPIState, &state.RedirectURI)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthAuthorizationState{}, ErrOAuthCredentialInvalid
	}
	if err != nil {
		return OAuthAuthorizationState{}, fmt.Errorf("consume OAuth authorization state: %w", err)
	}
	s.releaseOAuthCredential(oauthCredentialState)
	return state, nil
}

func (s *Store) CreateOAuthLoginCode(ctx context.Context, identity OAuthIdentity) (string, error) {
	if s == nil || s.database == nil || !validOAuthIdentity(identity) {
		return "", errors.New("valid OAuth identity is required")
	}
	now := s.currentTime()
	if err := s.reserveOAuthCredential(ctx, oauthCredentialLoginCode, now); err != nil {
		return "", err
	}
	reserved := true
	defer func() {
		if reserved {
			s.releaseOAuthCredential(oauthCredentialLoginCode)
		}
	}()
	raw, digest, err := generateOAuthCredential()
	if err != nil {
		return "", err
	}
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
	reserved = false
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
	now := s.currentTime()
	if err := s.reserveOAuthCredential(ctx, oauthCredentialAccessHandle, now); err != nil {
		return "", err
	}
	reservedAccessHandle := true
	defer func() {
		if reservedAccessHandle {
			s.releaseOAuthCredential(oauthCredentialAccessHandle)
		}
	}()
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin OAuth login code exchange: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	formattedNow := now.Format(time.RFC3339Nano)
	var identity OAuthIdentity
	err = tx.QueryRowContext(ctx, `
DELETE FROM oauth_login_codes
WHERE code_hash = ? AND consumed_at = '' AND expires_at > ?
	RETURNING subject, username, display_name`,
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
	reservedAccessHandle = false
	s.releaseOAuthCredential(oauthCredentialLoginCode)
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
DELETE FROM oauth_access_handles
WHERE handle_hash = ? AND consumed_at = '' AND expires_at > ?
RETURNING subject, username, display_name`,
		digest[:],
		now.UnixNano(),
	).Scan(&identity.Subject, &identity.Username, &identity.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthIdentity{}, ErrOAuthCredentialInvalid
	}
	if err != nil {
		return OAuthIdentity{}, fmt.Errorf("consume OAuth access handle: %w", err)
	}
	s.releaseOAuthCredential(oauthCredentialAccessHandle)
	return identity, nil
}

func (s *Store) currentTime() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func (s *Store) initializeOAuthCredentialRetention(ctx context.Context) error {
	now := s.currentTime()
	for kind, table := range oauthCredentialTables {
		if _, err := s.database.ExecContext(
			ctx,
			"DELETE FROM "+table+" WHERE expires_at <= ?",
			now.UnixNano(),
		); err != nil {
			return fmt.Errorf("initialize OAuth credential retention: %w", err)
		}
		if err := s.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).
			Scan(&s.oauthCounts[kind]); err != nil {
			return fmt.Errorf("count OAuth credentials: %w", err)
		}
	}
	s.oauthLastPrune = now
	return nil
}

func (s *Store) reserveOAuthCredential(
	ctx context.Context,
	kind oauthCredentialKind,
	now time.Time,
) error {
	if err := s.pruneExpiredOAuthCredentials(ctx, now); err != nil {
		return err
	}
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.oauthCounts[kind] >= oauthCredentialLimits[kind] {
		return ErrOAuthCredentialCapacity
	}
	s.oauthCounts[kind]++
	return nil
}

func (s *Store) releaseOAuthCredential(kind oauthCredentialKind) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.oauthCounts[kind] > 0 {
		s.oauthCounts[kind]--
	}
}

func (s *Store) pruneExpiredOAuthCredentials(ctx context.Context, now time.Time) error {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if !s.oauthLastPrune.IsZero() && !now.Before(s.oauthLastPrune) &&
		now.Sub(s.oauthLastPrune) < oauthCredentialPruneInterval {
		return nil
	}
	for kind, table := range oauthCredentialTables {
		result, err := s.database.ExecContext(
			ctx,
			"DELETE FROM "+table+" WHERE expires_at <= ?",
			now.UnixNano(),
		)
		if err != nil {
			return fmt.Errorf("prune expired OAuth credentials: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count pruned OAuth credentials: %w", err)
		}
		s.oauthCounts[kind] -= min(s.oauthCounts[kind], deleted)
	}
	s.oauthLastPrune = now
	return nil
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
