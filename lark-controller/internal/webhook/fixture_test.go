package webhook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func loadLarkFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join("..", "..", "testdata", "lark", name))
	if err != nil {
		t.Fatalf("read Lark fixture %q: %v", name, err)
	}
	var fixture map[string]any
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatalf("decode Lark fixture %q: %v", name, err)
	}
	return fixture
}
