// Package dnscheck resolves the records a user must create before a session
// can run on a domain they own: one TXT proving they control the zone, and four
// CNAMEs proving traffic reaches us.
//
// Queries go straight to public resolvers rather than through the cluster's
// CoreDNS. A user who presses Verify before creating a record has the NXDOMAIN
// negatively cached for the zone's SOA minimum — often 5 to 60 minutes — and
// caching that answer in our own resolver too would add a second, longer delay
// we could do nothing about.
package dnscheck

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// Resolvers queried in order; a host passes if either agrees. Two independent
// operators, so one being slow or wrong never blocks a verification.
var defaultResolverAddrs = []string{"1.1.1.1:53", "8.8.8.8:53"}

// defaultTimeout bounds one verification attempt. All five lookups run in
// parallel, so this is the wall-clock budget for the whole check.
const defaultTimeout = 5 * time.Second

// Result is one hostname's resolution.
type Result struct {
	FQDN string `json:"fqdn"`
	// CNAME is the final target of the alias chain, when the name is an alias.
	// Empty when the name resolves directly.
	CNAME string `json:"cname,omitempty"`
	// IPs is what the name ultimately resolves to, after following CNAMEs.
	IPs    []string `json:"resolved"`
	OK     bool     `json:"ok"`
	Detail string   `json:"detail,omitempty"`
}

// TxtResult is the challenge record's state. Found holds every value seen at
// either checked name, since a name legitimately carries several.
type TxtResult struct {
	Name     string   `json:"name"`
	Expected string   `json:"expected"`
	Found    []string `json:"found"`
	OK       bool     `json:"ok"`
	Detail   string   `json:"detail,omitempty"`
}

type Checker struct {
	resolvers []*net.Resolver
	timeout   time.Duration
}

// New returns a Checker querying the public resolvers above.
func New() *Checker {
	return NewWithResolvers(defaultResolverAddrs...)
}

// NewWithResolvers builds a Checker over specific resolver addresses
// ("host:port"). Exists for tests, which point it at a local stub.
func NewWithResolvers(addrs ...string) *Checker {
	resolvers := make([]*net.Resolver, 0, len(addrs))
	for _, addr := range addrs {
		resolvers = append(resolvers, newResolver(addr))
	}
	return &Checker{resolvers: resolvers, timeout: defaultTimeout}
}

// newResolver builds a resolver that ignores the host's own DNS configuration
// and talks to addr directly. PreferGo is what makes Dial take effect — without
// it cgo's resolver would use /etc/resolv.conf.
func newResolver(addr string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
	}
}

// CheckHosts resolves every hostname in parallel and reports whether each
// lands on wantIP. Order matches the input.
func (c *Checker) CheckHosts(ctx context.Context, fqdns []string, wantIP string) []Result {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	results := make([]Result, len(fqdns))
	var wg sync.WaitGroup
	for i, fqdn := range fqdns {
		wg.Go(func() { results[i] = c.checkHost(ctx, fqdn, wantIP) })
	}
	wg.Wait()
	return results
}

// checkHost resolves one name against every resolver and keeps the first
// answer that passes — or, if none do, the first that answered at all, so the
// detail describes what is actually published rather than a timeout.
func (c *Checker) checkHost(ctx context.Context, fqdn, wantIP string) Result {
	var fallback *Result
	for _, r := range c.resolvers {
		res := resolveHost(ctx, r, fqdn, wantIP)
		if res.OK {
			return res
		}
		if fallback == nil || (len(res.IPs) > 0 && len(fallback.IPs) == 0) {
			cp := res
			fallback = &cp
		}
	}
	if fallback == nil {
		return Result{FQDN: fqdn, Detail: "no resolver could be reached"}
	}
	return *fallback
}

func resolveHost(ctx context.Context, r *net.Resolver, fqdn, wantIP string) Result {
	res := Result{FQDN: fqdn}

	// The alias target is reported for the UI even when the addresses are
	// wrong — "points at the right name but the wrong IP" and "points at
	// something else entirely" are very different problems for the user.
	if cname, err := r.LookupCNAME(ctx, fqdn); err == nil {
		cname = strings.TrimSuffix(cname, ".")
		if !strings.EqualFold(cname, fqdn) {
			res.CNAME = cname
		}
	}

	addrs, err := r.LookupHost(ctx, fqdn)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			res.Detail = "no record found"
			return res
		}
		res.Detail = "lookup failed: " + err.Error()
		return res
	}

	// IPv6 is not part of the contract — we advertise an A record only — so
	// AAAA answers are reported but never decide the outcome.
	var v4 []string
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			v4 = append(v4, a)
		}
	}
	res.IPs = addrs

	if len(v4) == 0 {
		res.Detail = "no IPv4 address — the record must be an A record (or a CNAME leading to one)"
		return res
	}

	var wrong []string
	for _, ip := range v4 {
		if ip != wantIP {
			wrong = append(wrong, ip)
		}
	}
	if len(wrong) == 0 {
		res.OK = true
		return res
	}

	// A domain sitting behind an orange-cloud Cloudflare record resolves to
	// Cloudflare's anycast IPs and never reaches us, and the generic mismatch
	// message sends people looking in the wrong place entirely.
	if allCloudflare(wrong) {
		res.Detail = "points at Cloudflare's proxy — switch the record to DNS only (grey cloud)"
		return res
	}
	if len(wrong) == len(v4) {
		res.Detail = "resolves to " + strings.Join(wrong, ", ") + " — expected " + wantIP
		return res
	}
	res.Detail = "also resolves to " + strings.Join(wrong, ", ") + " — remove every address except " + wantIP
	return res
}

// CheckTXT looks for expected at the challenge name and at the apex, and
// passes if any value at either matches.
//
// Both locations are accepted because a handful of DNS panels cannot create
// underscore-prefixed labels; the extra lookup costs one round trip and removes
// a whole class of support request.
func (c *Checker) CheckTXT(ctx context.Context, challengeName, apex, expected string) TxtResult {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	out := TxtResult{Name: challengeName, Expected: expected}

	seen := make(map[string]bool)
	for _, name := range []string{challengeName, apex} {
		for _, r := range c.resolvers {
			values, err := r.LookupTXT(ctx, name)
			if err != nil {
				continue
			}
			for _, v := range values {
				if !seen[v] {
					seen[v] = true
					out.Found = append(out.Found, v)
				}
			}
		}
	}

	// Match if ANY value equals the token — never require it to be the only
	// one. A domain attached from both development and production carries two
	// challenge values, and an apex TXT routinely holds SPF, DMARC and other
	// vendors' tokens.
	if slices.Contains(out.Found, expected) {
		out.OK = true
		return out
	}

	switch {
	case len(out.Found) == 0:
		out.Detail = "no TXT record found at " + challengeName + " or " + apex
	default:
		out.Detail = "no TXT value matches — an older token may still be present"
	}
	return out
}

// Cloudflare's published IPv4 ranges (https://www.cloudflare.com/ips-v4).
// Used only to turn a mismatch into a more useful message, so a stale entry
// degrades the wording and never the verdict.
var cloudflareNets = mustParseCIDRs(
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}

// allCloudflare reports whether every address belongs to Cloudflare's edge.
// All of them, not any: one Cloudflare address alongside ours is a different
// misconfiguration than a fully proxied record.
func allCloudflare(ips []string) bool {
	if len(ips) == 0 {
		return false
	}
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			return false
		}
		inRange := false
		for _, n := range cloudflareNets {
			if n.Contains(ip) {
				inRange = true
				break
			}
		}
		if !inRange {
			return false
		}
	}
	return true
}
