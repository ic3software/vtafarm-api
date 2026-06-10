package setup

import (
	"fmt"
	"regexp"
	"strings"
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

const didLogMarker = "---DID_LOG_START---"

// ParseVtaDidLog extracts the did.jsonl content appended to the setup job logs.
// The setup job command appends the marker then cats the file, so everything
// after the marker is the raw JSONL content.
func ParseVtaDidLog(logs string) string {
	idx := strings.Index(logs, didLogMarker)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(logs[idx+len(didLogMarker):])
}
