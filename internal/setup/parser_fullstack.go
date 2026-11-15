package setup

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ic3software/vtafarm-api/internal/didresolver"
)

// ── §8: regex output parsing for the full_stack step Jobs ──────────────────

var (
	mediatorDIDRe        = regexp.MustCompile(`(?i)mediator:\s*(did:\S+)`)
	digestRe             = regexp.MustCompile(`SHA-256 digest:\s+(\S+)`)
	mediatorAdminDIDRe   = regexp.MustCompile(`Admin DID:\s+(did:\S+)`)
	webvhAdminDIDRe      = regexp.MustCompile(`Generated admin did:key:\s+(did:\S+)`)
	webvhAdminKeyRe      = regexp.MustCompile(`Private key \(save now, not re-shown\):\s+(\S+)`)
	serverDidRe          = regexp.MustCompile(`server_did\s*=\s*"([^"]+)"`)
	didsEnrollURLRe      = regexp.MustCompile(`(https?://\S+/enroll\S*)`)
	didsEnrollClaimRe    = regexp.MustCompile(`(?im)^[ \t]*Claim code:[ \t]*(?:\r?\n[ \t]*)?([A-Z0-9-]+)[ \t]*$`)
	artifactMarkerLineRe = regexp.MustCompile(`(?m)^---ARTIFACT:[^\n]*?---\s*$`)
	ansiEscapeRe         = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
)

type mediatorAdminProfile struct {
	Alias   string                `json:"alias"`
	DID     string                `json:"did"`
	Secrets []mediatorAdminSecret `json:"secrets"`
}

type mediatorAdminSecret struct {
	PrivateKeyJWK       *mediatorAdminJWK `json:"privateKeyJwk"`
	PrivateKeyMultibase string            `json:"privateKeyMultibase"`
}

type mediatorAdminJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	D   string `json:"d"`
	X   string `json:"x"`
}

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

// ParseMediatorAdminProfile extracts the generated admin key from the
// owner-only TDK profile written by mediator-setup. The profile reaches the API
// over pods/exec, never through the setup Job's logs.
func ParseMediatorAdminProfile(profileJSON string) (string, error) {
	var profile mediatorAdminProfile
	if err := json.Unmarshal([]byte(profileJSON), &profile); err != nil {
		return "", fmt.Errorf("invalid mediator admin profile: %w", err)
	}
	if profile.Alias != "admin" || !strings.HasPrefix(profile.DID, "did:key:") {
		return "", fmt.Errorf("mediator admin profile identity is invalid")
	}
	if len(profile.Secrets) != 1 {
		return "", fmt.Errorf("mediator admin profile has %d secrets, want 1", len(profile.Secrets))
	}
	key, did, err := mediatorAdminPrivateKey(profile.Secrets[0])
	if err != nil {
		return "", err
	}
	if profile.DID != did {
		return "", fmt.Errorf("mediator admin profile private key does not match its DID")
	}
	return key, nil
}

func mediatorAdminPrivateKey(secret mediatorAdminSecret) (key, did string, err error) {
	if secret.PrivateKeyJWK != nil && secret.PrivateKeyMultibase != "" {
		return "", "", fmt.Errorf("mediator admin profile contains ambiguous private key material")
	}

	var seed []byte
	if jwk := secret.PrivateKeyJWK; jwk != nil {
		if jwk.Kty != "OKP" || jwk.Crv != "Ed25519" {
			return "", "", fmt.Errorf("mediator admin profile private key is not Ed25519")
		}
		seed, err = decodeCanonicalBase64URL(jwk.D, ed25519.SeedSize)
		if err != nil {
			return "", "", fmt.Errorf("mediator admin profile has an invalid private key")
		}
		public, publicErr := decodeCanonicalBase64URL(jwk.X, ed25519.PublicKeySize)
		if publicErr != nil {
			clear(seed)
			return "", "", fmt.Errorf("mediator admin profile has an invalid public key")
		}
		derivedPublic := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
		if !ed25519.PublicKey(public).Equal(derivedPublic) {
			clear(seed)
			return "", "", fmt.Errorf("mediator admin profile public and private keys do not match")
		}
		key = "z" + didresolver.EncodeBase58BTC(append([]byte{0x80, 0x26}, seed...))
	} else if secret.PrivateKeyMultibase != "" {
		decoded, decodeErr := didresolver.DecodeBase58BTC(strings.TrimPrefix(secret.PrivateKeyMultibase, "z"))
		if decodeErr != nil || !strings.HasPrefix(secret.PrivateKeyMultibase, "z") || len(decoded) != 2+ed25519.SeedSize || decoded[0] != 0x80 || decoded[1] != 0x26 {
			return "", "", fmt.Errorf("mediator admin profile has an invalid private key")
		}
		if "z"+didresolver.EncodeBase58BTC(decoded) != secret.PrivateKeyMultibase {
			return "", "", fmt.Errorf("mediator admin profile has a non-canonical private key")
		}
		seed = append([]byte(nil), decoded[2:]...)
		key = secret.PrivateKeyMultibase
	} else {
		return "", "", fmt.Errorf("mediator admin profile has no private key")
	}
	defer clear(seed)

	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	did = "did:key:z" + didresolver.EncodeBase58BTC(append([]byte{0xed, 0x01}, public...))
	return key, did, nil
}

func decodeCanonicalBase64URL(value string, size int) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != size || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, fmt.Errorf("invalid base64url value")
	}
	return decoded, nil
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

// ParseDidsEnrollInvite extracts the admin-panel enrollment URL and the
// second-channel claim code from `did-hosting-daemon invite --role admin`.
// Current daemon images always issue both; accepting a link without its code
// would leave the user with an enrollment ceremony they cannot complete.
func ParseDidsEnrollInvite(output string) (enrollURL, claimCode string, err error) {
	if m := didsEnrollURLRe.FindStringSubmatch(output); m != nil {
		enrollURL = m[1]
	} else {
		return "", "", fmt.Errorf("dids enrollment URL not found in output")
	}
	if m := didsEnrollClaimRe.FindStringSubmatch(output); m != nil {
		claimCode = strings.TrimSpace(m[1])
	} else {
		return "", "", fmt.Errorf("dids enrollment claim code not found in output")
	}
	return enrollURL, claimCode, nil
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
