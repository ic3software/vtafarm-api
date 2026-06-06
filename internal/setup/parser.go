package setup

import (
	"fmt"
	"regexp"
)

var vtaDIDRe = regexp.MustCompile(`(?i)vta did:\s*(did:\S+)`)

// ParseVtaDID extracts the VTA DID from the output of `vta setup`.
// Expected line format: "VTA DID: did:webvh:..."
func ParseVtaDID(output string) (string, error) {
	if m := vtaDIDRe.FindStringSubmatch(output); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("VTA DID not found in setup output")
}
