package digest_test

import (
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/digest"
)

func TestIsCanonicalSHA256(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "lowercase hex", value: strings.Repeat("a", 64), want: true},
		{name: "uppercase hex", value: strings.Repeat("A", 64)},
		{name: "non-hex", value: strings.Repeat("z", 64)},
		{name: "short", value: strings.Repeat("a", 63)},
		{name: "long", value: strings.Repeat("a", 65)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := digest.IsCanonicalSHA256(test.value); got != test.want {
				t.Fatalf("IsCanonicalSHA256() = %t, want %t", got, test.want)
			}
		})
	}
}
