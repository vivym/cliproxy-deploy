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

func (s *GrantSealer) KeyID() string {
	if s == nil {
		return ""
	}
	return s.keyID
}

func LoadGrantPayloadKeyFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("grant payload key file is required")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("read grant payload key file")
	}
	encoded = bytes.TrimSuffix(encoded, []byte{'\n'})
	encoded = bytes.TrimSuffix(encoded, []byte{'\r'})
	if len(encoded) != grantPayloadKeyBytes*2 {
		return nil, errors.New("grant payload key file must contain one 64-character lowercase hex line")
	}
	key := make([]byte, grantPayloadKeyBytes)
	if _, err := hex.Decode(key, encoded); err != nil || !bytes.Equal([]byte(hex.EncodeToString(key)), encoded) {
		return nil, errors.New("grant payload key file must contain one 64-character lowercase hex line")
	}
	return key, nil
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
