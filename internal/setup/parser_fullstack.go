package setup

import (
	"fmt"
	"regexp"
	"strings"
)

// ── §8: regex output parsing for the full_stack step Jobs ──────────────────

var (
	mediatorDIDRe        = regexp.MustCompile(`(?i)mediator:\s*(did:\S+)`)
	digestRe             = regexp.MustCompile(`SHA-256 digest:\s+(\S+)`)
	mediatorAdminDIDRe   = regexp.MustCompile(`Admin DID:\s+(did:\S+)`)
	mediatorAdminKeyRe   = regexp.MustCompile(`Private key \(multibase\):\s+(\S+)`)
	webvhAdminDIDRe      = regexp.MustCompile(`Generated admin did:key:\s+(did:\S+)`)
	webvhAdminKeyRe      = regexp.MustCompile(`Private key \(save now, not re-shown\):\s+(\S+)`)
	serverDidRe          = regexp.MustCompile(`server_did\s*=\s*"([^"]+)"`)
	didsEnrollURLRe      = regexp.MustCompile(`(https?://\S+/enroll\S*)`)
	artifactMarkerLineRe = regexp.MustCompile(`(?m)^---ARTIFACT:[^\n]*?---\s*$`)
	ansiEscapeRe         = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
)

// stripANSI removes ANSI/CSI escape sequences (color codes, cursor moves)
// from CLI output. Some full_stack binaries colorize stdout even when piped
// to a Job's log (not a real TTY), which can leak a trailing "\x1b[0m" reset
// straight into a \S+-captured value (e.g. onto the end of an admin key).
// Applied to every Job's logs before parsing — see orchestrator_fullstack.go.
func stripANSI(s string) string {
	return ansiEscapeRe.ReplaceAllString(s, "")
}

// ParseMediatorDID extracts the mediator DID (1b) from `vta setup` output.
func ParseMediatorDID(output string) (string, error) {
	if m := mediatorDIDRe.FindStringSubmatch(output); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("mediator DID not found in setup output")
}

// ParseDigest extracts a "SHA-256 digest: <value>" line — used for both 2a
// (step_mediator_reprov) and 3a (step_dids_provision).
func ParseDigest(output string) (string, error) {
	if m := digestRe.FindStringSubmatch(output); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("SHA-256 digest not found in output")
}

// ParseMediatorAdminDID extracts the mediator admin DID (2b) from
// `vta contexts reprovision` output.
func ParseMediatorAdminDID(output string) (string, error) {
	if m := mediatorAdminDIDRe.FindStringSubmatch(output); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("mediator admin DID not found in output")
}

// ParseMediatorAdminKey extracts the mediator admin private key (2c,
// multibase) from `mediator-setup --bundle --digest` output.
func ParseMediatorAdminKey(output string) (string, error) {
	if m := mediatorAdminKeyRe.FindStringSubmatch(output); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("mediator admin private key not found in output")
}

// ParseWebvhAdminDID extracts the webvh admin DID (3b) from
// `did-hosting-daemon setup` (offline-complete) output.
func ParseWebvhAdminDID(output string) (string, error) {
	if m := webvhAdminDIDRe.FindStringSubmatch(output); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("webvh admin DID not found in output")
}

// ParseWebvhAdminKey extracts the webvh admin private key (3c, multibase)
// from `did-hosting-daemon setup` (offline-complete) output.
func ParseWebvhAdminKey(output string) (string, error) {
	if m := webvhAdminKeyRe.FindStringSubmatch(output); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("webvh admin private key not found in output")
}

// ParseServerDid extracts the daemon DID (3d) from the `server_did = "..."`
// line of config.toml (captured via the step_dids_p2 Job's
// `grep '^server_did' config.toml` artifact).
func ParseServerDid(artifact string) (string, error) {
	if m := serverDidRe.FindStringSubmatch(artifact); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("server_did not found in config.toml artifact")
}

// ParseDidsEnrollURL extracts the dids admin-panel enrollment URL (3e) from
// `did-hosting-daemon invite --role admin` output. The design doc doesn't
// pin down an exact line format beyond the response sketch
// (".../enroll/..."), so this is a best-effort pattern — unverified against
// the real binary's output.
func ParseDidsEnrollURL(output string) (string, error) {
	if m := didsEnrollURLRe.FindStringSubmatch(output); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("dids enrollment URL not found in output")
}

// ParseArtifact extracts the content following a "---ARTIFACT:<marker>---"
// line up to (but not including) the next "---ARTIFACT:...---" marker or end
// of output. Used for the full_stack Jobs that emit multiple artifacts in one
// command chain (step_vta_setup's two DID logs, step_dids_p2's server_did) —
// distinct from vta_only's single "---DID_LOG_START---" marker (ParseVtaDidLog),
// which is left as-is.
func ParseArtifact(logs, marker string) string {
	start := "---ARTIFACT:" + marker + "---"
	_, rest, found := strings.Cut(logs, start)
	if !found {
		return ""
	}
	if loc := artifactMarkerLineRe.FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	return strings.TrimSpace(rest)
}
