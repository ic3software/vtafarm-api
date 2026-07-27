package setup

import (
	"fmt"
	"regexp"
	"strings"
)

// ── §8/§13: regex output parsing for the full_stack Jobs ───────────
//
// Unlike full_stack's other parsed values, `vtc setup --from`'s terse
// completion block is machine-oriented key=value lines
// (print_setup_summary_terse), so one pass of anchored line regexes covers
// everything — nothing per-field to disambiguate. Callers strip ANSI first
// (fsJobLogs), same as every other full_stack parse.

var (
	// `vtc setup --setup-key-out`'s output is the shared
	// vta_sdk::provision_client::driver::run_phase1_init prose block (the same
	// one mediator-setup / did-hosting-daemon print), not a machine key=value
	// line — the DID appears indented on the line right after the
	// "Setup DID (ephemeral):" header.
	vtcSetupKeyDidRe = regexp.MustCompile(`Setup DID \(ephemeral\):\s+(did:\S+)`)
	vtcDidRe         = regexp.MustCompile(`(?m)^vtc_did=(.+)$`)
	vtcAdminDidRe    = regexp.MustCompile(`(?m)^admin_did=(.+)$`)
	vtcInstallURLRe  = regexp.MustCompile(`(?m)^install_url=(.+)$`)
	vtcClaimCodeRe   = regexp.MustCompile(`(?m)^claim_code=(.+)$`)

	// `vtc admin invite` (reissue-install) prints prose lines rather than the
	// terse block; the CLI writes them to stderr, but K8s Job logs capture
	// both streams.
	vtcInviteURLRe   = regexp.MustCompile(`Install URL \(one-shot\):\s+(\S+)`)
	vtcInviteClaimRe = regexp.MustCompile(`Claim code[^:\n]*:\s+(\S+)`)
)

// ParseVtcSetupKeyDid extracts the ephemeral setup key DID from
// `vtc setup --setup-key-out` output (design §4b/§8).
func ParseVtcSetupKeyDid(output string) (string, error) {
	if m := vtcSetupKeyDidRe.FindStringSubmatch(output); m != nil {
		return strings.TrimSpace(m[1]), nil
	}
	return "", fmt.Errorf("setup DID not found in setup --setup-key-out output")
}

// VtcSetupOutcome holds the values collected from `vtc setup --from`'s terse
// completion block. The block's config_path/data_dir lines are local paths
// inside the Job and are deliberately not captured.
type VtcSetupOutcome struct {
	VtcDid     string // the VTC's own did:webvh
	AdminDid   string // pre-claim install admin — NOT the PNM admin DID
	InstallURL string // one-shot admin install URL (15-min token TTL)
	ClaimCode  string // second-channel claim code
}

// ParseVtcSetupOutput extracts all fields of the terse completion block in
// one pass (design §8). Every field is required — a missing line means the
// setup didn't finish cleanly.
func ParseVtcSetupOutput(logs string) (VtcSetupOutcome, error) {
	var out VtcSetupOutcome
	for _, f := range []struct {
		re   *regexp.Regexp
		dst  *string
		name string
	}{
		{vtcDidRe, &out.VtcDid, "vtc_did"},
		{vtcAdminDidRe, &out.AdminDid, "admin_did"},
		{vtcInstallURLRe, &out.InstallURL, "install_url"},
		{vtcClaimCodeRe, &out.ClaimCode, "claim_code"},
	} {
		m := f.re.FindStringSubmatch(logs)
		if m == nil {
			return VtcSetupOutcome{}, fmt.Errorf("%s not found in vtc setup output", f.name)
		}
		*f.dst = strings.TrimSpace(m[1])
	}
	return out, nil
}

// ParseVtcInviteOutput extracts the reminted install URL and claim code from
// `vtc admin invite` output (design §13, reissue-install).
func ParseVtcInviteOutput(logs string) (installURL, claimCode string, err error) {
	m := vtcInviteURLRe.FindStringSubmatch(logs)
	if m == nil {
		return "", "", fmt.Errorf("install URL not found in vtc admin invite output")
	}
	installURL = strings.TrimSpace(m[1])
	m = vtcInviteClaimRe.FindStringSubmatch(logs)
	if m == nil {
		return "", "", fmt.Errorf("claim code not found in vtc admin invite output")
	}
	claimCode = strings.TrimSpace(m[1])
	return installURL, claimCode, nil
}
