package newapi_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

func TestGrantSealerRoundTripsCanonicalRequestAndRejectsTampering(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	sealer, err := newapi.NewGrantSealer(key)
	if err != nil {
		t.Fatalf("new grant sealer: %v", err)
	}
	request := validSubscriptionGrant()
	sealed, err := sealer.Seal(request)
	if err != nil {
		t.Fatalf("seal grant: %v", err)
	}
	if sealed.KeyID == "" || sealed.ExternalID != request.ExternalID ||
		sealed.RequestSHA256 == "" || len(sealed.Nonce) != 12 || len(sealed.Ciphertext) == 0 {
		t.Fatalf("incomplete sealed grant: %+v", sealed)
	}

	restarted, err := newapi.NewGrantSealer(append([]byte(nil), key...))
	if err != nil {
		t.Fatalf("restart grant sealer: %v", err)
	}
	opened, err := restarted.Open(sealed)
	if err != nil {
		t.Fatalf("open grant: %v", err)
	}
	if opened.ExternalID != request.ExternalID || opened.Identity.Subject != request.Identity.Subject ||
		opened.Grant.LevelCode != request.Grant.LevelCode || opened.Evidence == nil ||
		opened.Evidence.InstanceCode != request.Evidence.InstanceCode {
		t.Fatalf("unexpected opened grant: %+v", opened)
	}

	tests := []struct {
		name   string
		mutate func(*newapi.SealedGrantRequest)
	}{
		{name: "key id", mutate: func(value *newapi.SealedGrantRequest) { value.KeyID = strings.Repeat("0", 64) }},
		{name: "external id", mutate: func(value *newapi.SealedGrantRequest) { value.ExternalID += "-other" }},
		{name: "request hash", mutate: func(value *newapi.SealedGrantRequest) { value.RequestSHA256 = string(bytes.Repeat([]byte{'0'}, 64)) }},
		{name: "nonce", mutate: func(value *newapi.SealedGrantRequest) { value.Nonce[0] ^= 0xff }},
		{name: "ciphertext", mutate: func(value *newapi.SealedGrantRequest) { value.Ciphertext[0] ^= 0xff }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tampered := sealed
			tampered.Nonce = append([]byte(nil), sealed.Nonce...)
			tampered.Ciphertext = append([]byte(nil), sealed.Ciphertext...)
			test.mutate(&tampered)
			if _, err := restarted.Open(tampered); err == nil {
				t.Fatal("tampered grant opened successfully")
			}
		})
	}
}

func TestGrantSealerRequiresAES256Key(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		if _, err := newapi.NewGrantSealer(make([]byte, size)); err == nil {
			t.Fatalf("accepted %d-byte key", size)
		}
	}
}

func TestGrantSealerRejectsAuthenticatedNonCanonicalJSON(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	sealer, err := newapi.NewGrantSealer(key)
	if err != nil {
		t.Fatalf("new grant sealer: %v", err)
	}
	request := validSubscriptionGrant()
	canonical, err := sealer.Seal(request)
	if err != nil {
		t.Fatalf("seal canonical grant: %v", err)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal grant: %v", err)
	}
	payload = append([]byte(" \n"), payload...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("new AES cipher: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new GCM: %v", err)
	}
	nonce := bytes.Repeat([]byte{0x24}, aead.NonceSize())
	nonCanonical := newapi.SealedGrantRequest{
		KeyID: canonical.KeyID, ExternalID: canonical.ExternalID,
		RequestSHA256: canonical.RequestSHA256, Nonce: nonce,
	}
	nonCanonical.Ciphertext = aead.Seal(
		nil,
		nonce,
		payload,
		testGrantPayloadAAD(nonCanonical.ExternalID, nonCanonical.RequestSHA256),
	)
	if _, err := sealer.Open(nonCanonical); err == nil {
		t.Fatal("authenticated non-canonical grant opened successfully")
	}
}

func TestLoadGrantPayloadKeyFileAcceptsOneLowerHexLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grant-payload.key")
	if err := os.WriteFile(path, []byte(strings.Repeat("42", 32)+"\n"), 0o600); err != nil {
		t.Fatalf("write grant payload key: %v", err)
	}
	key, err := newapi.LoadGrantPayloadKeyFile(path)
	if err != nil {
		t.Fatalf("load grant payload key: %v", err)
	}
	if !bytes.Equal(key, bytes.Repeat([]byte{0x42}, 32)) {
		t.Fatalf("loaded key has unexpected value")
	}

	for name, value := range map[string]string{
		"too short":      strings.Repeat("42", 31),
		"uppercase":      strings.Repeat("AB", 32),
		"leading space":  " " + strings.Repeat("42", 32),
		"multiple lines": strings.Repeat("42", 32) + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				t.Fatalf("write invalid grant payload key: %v", err)
			}
			if _, err := newapi.LoadGrantPayloadKeyFile(path); err == nil ||
				strings.Contains(err.Error(), value) {
				t.Fatalf("invalid key error = %v", err)
			}
		})
	}
}

func testGrantPayloadAAD(externalID, requestSHA256 string) []byte {
	aad := make([]byte, 8+len(externalID)+len(requestSHA256))
	binary.BigEndian.PutUint32(aad[:4], uint32(len(externalID)))
	copy(aad[4:], externalID)
	offset := 4 + len(externalID)
	binary.BigEndian.PutUint32(aad[offset:offset+4], uint32(len(requestSHA256)))
	copy(aad[offset+4:], requestSHA256)
	return aad
}
