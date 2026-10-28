package setup

import (
	"encoding/json"
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

// Match the public did:webvh resolver's maximum response size, or a generated
// external log could be downloadable but impossible to validate.
const MaxVtaDIDLogBytes = 1 << 20

// ParseVtaDidLog extracts the did.jsonl content appended to the setup job logs.
func ParseVtaDidLog(logs string) string {
	idx := strings.Index(logs, didLogMarker)
	if idx < 0 {
		return ""
	}
	return normalizeVtaDidLog(logs[idx+len(didLogMarker):])
}

// normalizeVtaDidLog discards CLI output that may arrive after the marker when
// the container runtime merges stdout and stderr. Once a DID log entry starts,
// every non-empty line must remain valid JSONL so diagnostics are never sent to
// DID hosting as part of the log.
func normalizeVtaDidLog(output string) string {
	lines := strings.Split(output, "\n")
	entries := make([]string, 0, len(lines))
	started := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !isVtaDidLogEntry(line) {
			if started {
				return ""
			}
			continue
		}
		started = true
		entries = append(entries, line)
	}
	return strings.Join(entries, "\n")
}

func isVtaDidLogEntry(line string) bool {
	var entry struct {
		VersionID string `json:"versionId"`
	}
	return json.Unmarshal([]byte(line), &entry) == nil && entry.VersionID != ""
}
