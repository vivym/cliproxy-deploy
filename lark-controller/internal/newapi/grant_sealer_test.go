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

func TestGrantKeyringRotatesPrimaryWhileOpeningPreviousCiphertext(t *testing.T) {
	oldKey := bytes.Repeat([]byte{0x24}, 32)
	newKey := bytes.Repeat([]byte{0x42}, 32)
	oldKeyring, err := newapi.NewGrantKeyring(oldKey)
	if err != nil {
		t.Fatalf("new old grant keyring: %v", err)
	}
	request := validSubscriptionGrant()
	oldSealed, err := oldKeyring.Seal(request)
	if err != nil {
		t.Fatalf("seal with old grant key: %v", err)
	}

	rotated, err := newapi.NewGrantKeyring(newKey, oldKey)
	if err != nil {
		t.Fatalf("new rotated grant keyring: %v", err)
	}
	keyIDs := rotated.KeyIDs()
	if len(keyIDs) != 2 || keyIDs[0] != rotated.PrimaryKeyID() ||
		keyIDs[0] == oldSealed.KeyID || keyIDs[1] != oldSealed.KeyID {
		t.Fatalf("unexpected rotated grant key IDs: %v", keyIDs)
	}
	if opened, err := rotated.Open(oldSealed); err != nil || opened.ExternalID != request.ExternalID {
		t.Fatalf("open old ciphertext after rotation: opened=%+v err=%v", opened, err)
	}
	newSealed, err := rotated.Seal(request)
	if err != nil {
		t.Fatalf("seal with rotated primary key: %v", err)
	}
	if newSealed.KeyID != rotated.PrimaryKeyID() || newSealed.KeyID == oldSealed.KeyID {
		t.Fatalf("rotated seal used key %q, want new primary %q", newSealed.KeyID, rotated.PrimaryKeyID())
	}

	retired, err := newapi.NewGrantKeyring(newKey)
	if err != nil {
		t.Fatalf("new retired grant keyring: %v", err)
	}
	if _, err := retired.Open(oldSealed); err == nil || strings.Contains(err.Error(), oldSealed.KeyID) {
		t.Fatalf("retired key opened old ciphertext or leaked key ID: %v", err)
	}
	if _, err := retired.Open(newSealed); err != nil {
		t.Fatalf("retired keyring did not open new ciphertext: %v", err)
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

func TestLoadGrantPayloadKeyringFileSupportsStrictRotationFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grant-payload.keyring")
	primaryLine := strings.Repeat("42", 32)
	previousLine := strings.Repeat("24", 32)
	if err := os.WriteFile(path, []byte(primaryLine+"\r\n"+previousLine+"\r\n"), 0o600); err != nil {
		t.Fatalf("write grant payload keyring: %v", err)
	}
	keyring, err := newapi.LoadGrantPayloadKeyringFile(path)
	if err != nil {
		t.Fatalf("load grant payload keyring: %v", err)
	}
	primary, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new expected primary sealer: %v", err)
	}
	previous, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatalf("new previous sealer: %v", err)
	}
	if keyring.PrimaryKeyID() != primary.KeyID() || len(keyring.KeyIDs()) != 2 {
		t.Fatalf("unexpected loaded keyring IDs: %v", keyring.KeyIDs())
	}
	oldSealed, err := previous.Seal(validSubscriptionGrant())
	if err != nil {
		t.Fatalf("seal previous grant: %v", err)
	}
	if _, err := keyring.Open(oldSealed); err != nil {
		t.Fatalf("loaded keyring did not open previous grant: %v", err)
	}

	if err := os.WriteFile(path, []byte(primaryLine), 0o600); err != nil {
		t.Fatalf("write one-line compatible keyring: %v", err)
	}
	if one, err := newapi.LoadGrantPayloadKeyringFile(path); err != nil || len(one.KeyIDs()) != 1 {
		t.Fatalf("load one-line compatible keyring: keys=%v err=%v", one.KeyIDs(), err)
	}

	for name, value := range map[string]string{
		"empty":                      "",
		"too short":                  strings.Repeat("42", 31),
		"uppercase":                  strings.Repeat("AB", 32),
		"leading space":              " " + primaryLine,
		"blank line":                 primaryLine + "\n\n" + previousLine,
		"duplicate":                  primaryLine + "\n" + primaryLine,
		"bare carriage return":       primaryLine + "\r",
		"bare carriage return split": primaryLine + "\r" + previousLine,
		"mixed line endings":         primaryLine + "\r\n" + previousLine + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				t.Fatalf("write invalid grant payload keyring: %v", err)
			}
			if _, err := newapi.LoadGrantPayloadKeyringFile(path); err == nil ||
				(value != "" && strings.Contains(err.Error(), value)) {
				t.Fatalf("invalid keyring error = %v", err)
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
