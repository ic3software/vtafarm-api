package publicid

import (
	"strings"
	"testing"
)

func TestNewAccountFormat(t *testing.T) {
	for range 100 {
		value, err := NewAccount()
		if err != nil {
			t.Fatal(err)
		}
		if len(value) != 8 {
			t.Fatalf("len(%q) = %d, want 8", value, len(value))
		}
		for _, char := range value {
			if !strings.ContainsRune(userAlphabet, char) {
				t.Fatalf("NewAccount() = %q, contains unexpected character %q", value, char)
			}
		}
	}
}
