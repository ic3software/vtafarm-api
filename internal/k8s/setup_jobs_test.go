package k8s

import (
	"os/exec"
	"testing"
)

func TestQuoteShellArgPreservesUntrustedDID(t *testing.T) {
	value := "did:webvh:example.com:person'; printf injected; #"
	out, err := exec.Command("sh", "-c", "printf '%s' "+quoteShellArg(value)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != value {
		t.Fatalf("shell argument = %q, want %q", out, value)
	}
}
