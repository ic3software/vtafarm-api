package setup

import (
	"fmt"
	"regexp"
)

// Subdomains are derived from user-chosen names rather than random IDs:
// vta[-local]-<name> / mediator[-local]-<name> / dids[-local]-<name> all share
// the session's vta_name so they're recognizable as one session, and
// vtc[-local]-<name> uses the session's own vtc_name. In development each gets
// a "-local-" infix to distinguish local DNS records from production.

// The longest component prefix is "mediator-local-" (15 chars); DNS labels max
// out at 63, which leaves 48 for the name itself.
const maxNameLength = 48

// Alphanumeric runs joined by single hyphens — no leading/trailing hyphen and
// no "--" (consecutive hyphens are DNS-legal but IDNA reserves the "??--"
// form for punycode, so reject them outright rather than mint lookalikes).
var namePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidateName checks that a user-chosen component name (vta_name / vtc_name)
// can be embedded in a DNS label: lowercase letters and digits, with single
// hyphens between them, at most 48 chars.
func ValidateName(name string) error {
	if len(name) > maxNameLength {
		return fmt.Errorf("must be at most %d characters", maxNameLength)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("Only lowercase letters, digits, and hyphens are allowed. Consecutive hyphens (--) are not permitted, and names must start and end with a letter or digit")
	}
	return nil
}

func componentHost(env, component, name string) string {
	mid := "-"
	if env == "development" {
		mid = "-local-"
	}
	return component + mid + name
}

// VtaHost returns the vta_only subdomain prefix, vta[-local]-<vtaName>.
func VtaHost(env, vtaName string) string {
	return componentHost(env, "vta", vtaName)
}

// FullStackHosts derives the three full_stack subdomains (vta, mediator,
// dids) under domain from the session's vta_name: vta[-local]-<name> /
// mediator[-local]-<name> / dids[-local]-<name>.
func FullStackHosts(env, vtaName string) (vtaSub, mediatorSub, didsSub string) {
	return componentHost(env, "vta", vtaName),
		componentHost(env, "mediator", vtaName),
		componentHost(env, "dids", vtaName)
}

// FullStackWithVtcHosts derives the four full_stack_with_vtc subdomains — the
// same three FullStackHosts produces plus vtc[-local]-<vtcName>, which uses
// the VTC's own name rather than the vta_name.
func FullStackWithVtcHosts(env, vtaName, vtcName string) (vtaSub, mediatorSub, didsSub, vtcSub string) {
	vtaSub, mediatorSub, didsSub = FullStackHosts(env, vtaName)
	return vtaSub, mediatorSub, didsSub, componentHost(env, "vtc", vtcName)
}

// DID path components (did:webvh:<scid>:<dids host>:<path>) for the DIDs
// hosted on the session's own dids daemon. Every producer must agree on
// these: the webvh URLs rendered into the setup TOMLs mint the DIDs with
// this path, and step_dids_load_did's `load-did --path` must load each log
// at the SAME path or webvh resolution 404s (the daemon serves the log at
// the --path value, while resolvers derive the URL from the DID identifier).
func VtaDidPath(vtaName string) string      { return vtaName + "-vta" }
func MediatorDidPath(vtaName string) string { return vtaName + "-mediator" }
func VtcDidPath(vtcName string) string      { return vtcName + "-vtc" }
