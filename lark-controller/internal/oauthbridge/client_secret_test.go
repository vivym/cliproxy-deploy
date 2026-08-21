package oauthbridge_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/oauthbridge"
)

func TestLoadBridgeClientSecretFileAcceptsOnePrintableToken(t *testing.T) {
	secret := strings.Repeat("s", 32)
	for _, ending := range []string{"", "\n", "\r\n"} {
		path := filepath.Join(t.TempDir(), "bridge-client-secret")
		if err := os.WriteFile(path, []byte(secret+ending), 0o600); err != nil {
			t.Fatalf("write bridge client secret: %v", err)
		}
		loaded, err := oauthbridge.LoadClientSecretFile(path)
		if err != nil || loaded != secret {
			t.Fatalf("load ending %q: secret=%q err=%v", ending, loaded, err)
		}
	}
}

func TestLoadBridgeClientSecretFileRejectsMalformedOrMissingSecret(t *testing.T) {
	valid := strings.Repeat("s", 32)
	for name, contents := range map[string]string{
		"empty":          "",
		"short":          "short",
		"space":          valid + " ",
		"bare carriage":  valid + "\r",
		"multiple lines": valid + "\nextra",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bridge-client-secret")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("write malformed bridge client secret: %v", err)
			}
			_, err := oauthbridge.LoadClientSecretFile(path)
			if err == nil || (contents != "" && strings.Contains(err.Error(), contents)) {
				t.Fatalf("malformed secret error=%v, want redacted rejection", err)
			}
		})
	}
	if _, err := oauthbridge.LoadClientSecretFile(""); err == nil {
		t.Fatal("empty bridge client secret path was accepted")
	}
}
