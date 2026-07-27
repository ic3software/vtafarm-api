package setup

import (
	"fmt"
	"regexp"
)

// Subdomains are derived from user-chosen names rather than random IDs:
// vta-<name> / mediator-<name> / dids-<name> all share the session's vta_name
// so they're recognizable as one session, and vtc-<name> uses the session's
// own vtc_name. In development every label additionally carries a "dev-"
// prefix — see EnvPrefix.

// The longest component prefix is "dev-mediator-" (13 chars); DNS labels max
// out at 63, which leaves 50 for the name itself. The limit stays 48 — it was
// exact when the environment marker was the longer "-local-" infix and is now
// merely conservative, and holding it steady means no existing name stops
// validating.
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

// EnvPrefix returns the label prefix marking records created by a locally run
// API: "dev-" in development, "" everywhere else. It's a prefix rather than the
// "-local-" infix it replaced so every dev record sorts and greps together in
// the Cloudflare dashboard — the whole reason the marker exists.
func EnvPrefix(env string) string {
	if env == "development" {
		return "dev-"
	}
	return ""
}

// componentHost builds a component's DNS label. An empty name selects the
// fixed-label form ("dev-vta" rather than "dev-vta-<name>") that custom and
// platform domains use, where the four labels are the same for every session
// and the user-chosen name carries no hostname meaning.
func componentHost(env, component, name string) string {
	if name == "" {
		return EnvPrefix(env) + component
	}
	return EnvPrefix(env) + component + "-" + name
}

// CNAMETarget returns the hostname a custom domain's four CNAMEs point at:
// lb.<clusterDomain>, or dev-lb.<clusterDomain> in development. Both records
// must exist in the zone as grey-cloud (DNS only) A records to the cluster
// ingress — a proxied one answers other people's hostnames with Cloudflare
// error 1014.
//
// Users are sent at a name we control rather than at CLUSTER_INGRESS_IP
// directly because their records are effectively permanent — the DID-hosting
// hostname is baked into every did:webvh the session mints — so the cluster IP
// has to be able to change without anyone editing their DNS again.
func CNAMETarget(env, clusterDomain string) string {
	return EnvPrefix(env) + "lb." + clusterDomain
}

// VtaHost returns the vta_only subdomain prefix, [dev-]vta-<vtaName>.
func VtaHost(env, vtaName string) string {
	return componentHost(env, "vta", vtaName)
}

// FullStackHosts derives the four full_stack subdomains under domain:
// [dev-]vta-<vtaName> / [dev-]mediator-<vtaName> / [dev-]dids-<vtaName> all
// share the session's vta_name, while the VTC uses its own vtc_name so the
// community's URL reads independently of the VTA it sits on.
func FullStackHosts(env, vtaName, vtcName string) (vtaSub, mediatorSub, didsSub, vtcSub string) {
	return componentHost(env, "vta", vtaName),
		componentHost(env, "mediator", vtaName),
		componentHost(env, "dids", vtaName),
		componentHost(env, "vtc", vtcName)
}

// FixedHosts derives the four labels shared by custom and platform domains:
// vta / mediator / dids / vtc, each with the environment prefix and nothing
// else. There is no user-chosen name in them, which is precisely why one
// domain backs one session — a second would want the same four hostnames.
func FixedHosts(env string) (vtaSub, mediatorSub, didsSub, vtcSub string) {
	return componentHost(env, "vta", ""),
		componentHost(env, "mediator", ""),
		componentHost(env, "dids", ""),
		componentHost(env, "vtc", "")
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
