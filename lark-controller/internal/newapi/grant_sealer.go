package newapi

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

const grantPayloadKeyBytes = 32

type SealedGrantRequest struct {
	KeyID         string
	ExternalID    string
	RequestSHA256 string
	Nonce         []byte
	Ciphertext    []byte
}

type GrantSealer struct {
	aead  cipher.AEAD
	keyID string
}

type GrantKeyring struct {
	primary *GrantSealer
	sealers map[string]*GrantSealer
	keyIDs  []string
}

func NewGrantSealer(key []byte) (*GrantSealer, error) {
	if len(key) != grantPayloadKeyBytes {
		return nil, errors.New("grant payload key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(append([]byte(nil), key...))
	if err != nil {
		return nil, errors.New("initialize grant payload cipher")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("initialize grant payload sealer")
	}
	digest := sha256.Sum256(key)
	return &GrantSealer{aead: aead, keyID: hex.EncodeToString(digest[:])}, nil
}

func NewGrantKeyring(keys ...[]byte) (*GrantKeyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("grant payload keyring requires at least one key")
	}
	keyring := &GrantKeyring{
		sealers: make(map[string]*GrantSealer, len(keys)),
		keyIDs:  make([]string, 0, len(keys)),
	}
	for _, key := range keys {
		sealer, err := NewGrantSealer(key)
		if err != nil {
			return nil, err
		}
		if _, duplicate := keyring.sealers[sealer.KeyID()]; duplicate {
			return nil, errors.New("grant payload keyring contains a duplicate key")
		}
		if keyring.primary == nil {
			keyring.primary = sealer
		}
		keyring.sealers[sealer.KeyID()] = sealer
		keyring.keyIDs = append(keyring.keyIDs, sealer.KeyID())
	}
	return keyring, nil
}

func (k *GrantKeyring) PrimaryKeyID() string {
	if k == nil || k.primary == nil {
		return ""
	}
	return k.primary.KeyID()
}

func (k *GrantKeyring) KeyIDs() []string {
	if k == nil {
		return nil
	}
	return append([]string(nil), k.keyIDs...)
}

func (k *GrantKeyring) Seal(request EntitlementGrantRequest) (SealedGrantRequest, error) {
	if k == nil || k.primary == nil {
		return SealedGrantRequest{}, errors.New("grant payload keyring is required")
	}
	return k.primary.Seal(request)
}

func (k *GrantKeyring) Open(sealed SealedGrantRequest) (EntitlementGrantRequest, error) {
	if k == nil || k.sealers == nil {
		return EntitlementGrantRequest{}, errors.New("grant payload keyring is required")
	}
	sealer, ok := k.sealers[sealed.KeyID]
	if !ok {
		return EntitlementGrantRequest{}, errors.New("sealed grant payload uses an unavailable key")
	}
	return sealer.Open(sealed)
}

func (s *GrantSealer) KeyID() string {
	if s == nil {
		return ""
	}
	return s.keyID
}

func LoadGrantPayloadKeyringFile(path string) (*GrantKeyring, error) {
	if path == "" {
		return nil, errors.New("grant payload keyring file is required")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("read grant payload keyring file")
	}
	defer func() {
		for index := range encoded {
			encoded[index] = 0
		}
	}()
	if len(encoded) == 0 {
		return nil, errors.New("grant payload keyring file must contain at least one key")
	}
	lines, validLineEndings := splitGrantPayloadKeyringLines(encoded)
	if !validLineEndings {
		return nil, errors.New("grant payload keyring file must use LF or CRLF line endings")
	}
	keys := make([][]byte, 0, len(lines))
	defer func() {
		for _, key := range keys {
			for index := range key {
				key[index] = 0
			}
		}
	}()
	for _, line := range lines {
		key := make([]byte, grantPayloadKeyBytes)
		keys = append(keys, key)
		if !isLowerHexGrantPayloadKey(line) {
			return nil, errors.New("grant payload keyring file must contain 64-character lowercase hex lines")
		}
		_, _ = hex.Decode(key, line)
	}
	return NewGrantKeyring(keys...)
}

func splitGrantPayloadKeyringLines(encoded []byte) ([][]byte, bool) {
	separator := []byte{'\n'}
	if bytes.IndexByte(encoded, '\r') >= 0 {
		for index, character := range encoded {
			switch character {
			case '\r':
				if index+1 >= len(encoded) || encoded[index+1] != '\n' {
					return nil, false
				}
			case '\n':
				if index == 0 || encoded[index-1] != '\r' {
					return nil, false
				}
			}
		}
		separator = []byte{'\r', '\n'}
	}
	lines := bytes.Split(encoded, separator)
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines, len(lines) > 0
}

func isLowerHexGrantPayloadKey(encoded []byte) bool {
	if len(encoded) != grantPayloadKeyBytes*2 {
		return false
	}
	for _, character := range encoded {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func (s *GrantSealer) Seal(request EntitlementGrantRequest) (SealedGrantRequest, error) {
	if s == nil || s.aead == nil {
		return SealedGrantRequest{}, errors.New("grant payload sealer is required")
	}
	payload, requestSHA256, err := canonicalizeGrantRequest(request)
	if err != nil {
		return SealedGrantRequest{}, errors.New("invalid grant payload")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return SealedGrantRequest{}, errors.New("generate grant payload nonce")
	}
	sealed := SealedGrantRequest{
		KeyID:         s.keyID,
		ExternalID:    request.ExternalID,
		RequestSHA256: requestSHA256,
		Nonce:         nonce,
	}
	sealed.Ciphertext = s.aead.Seal(
		nil,
		sealed.Nonce,
		payload,
		grantPayloadAAD(sealed.ExternalID, sealed.RequestSHA256),
	)
	return sealed, nil
}

func (s *GrantSealer) Open(sealed SealedGrantRequest) (EntitlementGrantRequest, error) {
	if s == nil || s.aead == nil {
		return EntitlementGrantRequest{}, errors.New("grant payload sealer is required")
	}
	if sealed.KeyID != s.keyID || sealed.ExternalID == "" ||
		len(sealed.RequestSHA256) != sha256.Size*2 || len(sealed.Nonce) != s.aead.NonceSize() ||
		len(sealed.Ciphertext) < s.aead.Overhead() {
		return EntitlementGrantRequest{}, errors.New("invalid sealed grant payload")
	}
	if _, err := hex.DecodeString(sealed.RequestSHA256); err != nil {
		return EntitlementGrantRequest{}, errors.New("invalid sealed grant payload")
	}
	payload, err := s.aead.Open(
		nil,
		sealed.Nonce,
		sealed.Ciphertext,
		grantPayloadAAD(sealed.ExternalID, sealed.RequestSHA256),
	)
	if err != nil {
		return EntitlementGrantRequest{}, errors.New("authenticate sealed grant payload")
	}
	var request EntitlementGrantRequest
	if err := decodeStrictJSON(bytes.NewReader(payload), &request); err != nil {
		return EntitlementGrantRequest{}, errors.New("decode sealed grant payload")
	}
	canonical, requestSHA256, err := canonicalizeGrantRequest(request)
	if err != nil {
		return EntitlementGrantRequest{}, errors.New("invalid sealed grant payload")
	}
	if !bytes.Equal(payload, canonical) || request.ExternalID != sealed.ExternalID ||
		requestSHA256 != sealed.RequestSHA256 {
		return EntitlementGrantRequest{}, errors.New("sealed grant payload metadata mismatch")
	}
	return request, nil
}

func grantPayloadAAD(externalID, requestSHA256 string) []byte {
	aad := make([]byte, 8+len(externalID)+len(requestSHA256))
	binary.BigEndian.PutUint32(aad[:4], uint32(len(externalID)))
	copy(aad[4:], externalID)
	offset := 4 + len(externalID)
	binary.BigEndian.PutUint32(aad[offset:offset+4], uint32(len(requestSHA256)))
	copy(aad[offset+4:], requestSHA256)
	return aad
}
