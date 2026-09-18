package webvh

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/idna"

	"github.com/ic3software/vtafarm-api/internal/siop"
)

const (
	DefaultMaxResponseBytes = 1 << 20
	DefaultTimeout          = 5 * time.Second
)

type httpClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Resolver fetches and cryptographically validates a did:webvh v1.0 history
// before selecting the exact authentication key named by kid.
type Resolver struct {
	client           httpClient
	lookupIP         func(context.Context, string) ([]net.IPAddr, error)
	maxResponseBytes int64
	now              func() time.Time
	clockSkew        time.Duration
}

// NewResolver constructs a resolver with normal TLS verification, no
// redirects, public-address-only dialing, and bounded response/time limits.
func NewResolver(timeout time.Duration) *Resolver {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, resolved := range addresses {
			if !isPublicAddress(resolved.IP) {
				continue
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
		}
		return nil, errors.New("DID host has no public IP address")
	}

	return &Resolver{
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		lookupIP:         net.DefaultResolver.LookupIPAddr,
		maxResponseBytes: DefaultMaxResponseBytes,
		now:              time.Now,
		clockSkew:        siop.DefaultClockSkew,
	}
}

func (r *Resolver) ResolveAuthenticationKey(ctx context.Context, did, kid string) (ed25519.PublicKey, error) {
	if r == nil || r.client == nil || r.lookupIP == nil || r.now == nil || r.maxResponseBytes <= 0 || r.clockSkew < 0 {
		return nil, errors.New("did:webvh resolver is not configured")
	}
	logURL, host, err := resolutionURL(did)
	if err != nil {
		return nil, err
	}
	addresses, err := r.lookupIP(ctx, host)
	if err != nil {
		return nil, errors.New("DID host DNS lookup failed")
	}
	if len(addresses) == 0 {
		return nil, errors.New("DID host has no addresses")
	}
	for _, address := range addresses {
		if !isPublicAddress(address.IP) {
			return nil, errors.New("DID host resolves to a non-public address")
		}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, logURL.String(), nil)
	if err != nil {
		return nil, errors.New("create DID log request")
	}
	request.Header.Set("Accept", "application/jsonl, application/x-jsonlines;q=0.9")
	response, err := r.client.Do(request)
	if err != nil {
		return nil, errors.New("fetch DID log")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("DID log returned HTTP status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, r.maxResponseBytes+1))
	if err != nil {
		return nil, errors.New("read DID log")
	}
	if int64(len(body)) > r.maxResponseBytes {
		return nil, errors.New("DID log exceeds the response limit")
	}

	document, err := validateLog(body, did, r.now(), r.clockSkew)
	if err != nil {
		return nil, fmt.Errorf("validate DID log: %w", err)
	}
	return siop.AuthenticationKeyFromDocument(document, did, kid)
}

func resolutionURL(did string) (*url.URL, string, error) {
	if len(did) > 2048 {
		return nil, "", errors.New("did:webvh identifier is too long")
	}
	rest, ok := strings.CutPrefix(did, "did:webvh:")
	if !ok || strings.ContainsAny(did, "?#") {
		return nil, "", errors.New("invalid did:webvh identifier")
	}
	parts := strings.Split(rest, ":")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return nil, "", errors.New("invalid did:webvh identifier")
	}
	if _, err := parseSHA256Multihash(parts[0]); err != nil {
		return nil, "", errors.New("invalid did:webvh SCID")
	}
	hostPort, err := url.PathUnescape(parts[1])
	if err != nil || strings.ContainsAny(hostPort, "/?#@") {
		return nil, "", errors.New("invalid did:webvh host")
	}

	host := hostPort
	port := ""
	if strings.Contains(hostPort, ":") {
		host, port, err = net.SplitHostPort(hostPort)
		if err != nil || host == "" {
			return nil, "", errors.New("invalid did:webvh host and port")
		}
		portNumber, parseErr := strconv.ParseUint(port, 10, 16)
		if parseErr != nil || portNumber == 0 {
			return nil, "", errors.New("invalid did:webvh port")
		}
	}
	if net.ParseIP(host) != nil || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return nil, "", errors.New("IP literals and localhost are not valid did:webvh hosts")
	}
	host, err = idna.Lookup.ToASCII(strings.ToLower(host))
	if err != nil || host == "" || strings.Contains(host, "..") {
		return nil, "", errors.New("invalid did:webvh domain")
	}

	path := "/.well-known/did.jsonl"
	if len(parts) > 2 {
		segments := make([]string, 0, len(parts)-2)
		for _, part := range parts[2:] {
			decoded, err := url.PathUnescape(part)
			if err != nil || decoded == "" || decoded == "." || decoded == ".." || strings.Contains(decoded, "/") {
				return nil, "", errors.New("invalid did:webvh path")
			}
			segments = append(segments, decoded)
		}
		path = "/" + strings.Join(segments, "/") + "/did.jsonl"
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	}
	return &url.URL{Scheme: "https", Host: authority, Path: path}, host, nil
}

var blockedAddressPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func isPublicAddress(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range blockedAddressPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var _ siop.AuthenticationKeyResolver = (*Resolver)(nil)
