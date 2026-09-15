package webvh

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type clientFunc func(*http.Request) (*http.Response, error)

func (f clientFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

func testResolver(client httpClient, lookup func(context.Context, string) ([]net.IPAddr, error)) *Resolver {
	return &Resolver{
		client:           client,
		lookupIP:         lookup,
		maxResponseBytes: 32,
		now:              func() time.Time { return time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC) },
		clockSkew:        time.Minute,
	}
}

func TestResolverRejectsNonPublicDNSBeforeHTTP(t *testing.T) {
	called := false
	resolver := testResolver(
		clientFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return nil, errors.New("must not be called")
		}),
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
		},
	)
	did := "did:webvh:Qmetio9KXzDkPXDpSQVyXSTcPVvj5ysHgMZt7y5ffRNDzD:internal.example"
	_, err := resolver.ResolveAuthenticationKey(context.Background(), did, did+"#key-0")
	if err == nil || called {
		t.Fatalf("error = %v, HTTP called = %v", err, called)
	}
}

func TestResolverBoundsResponseAndRejectsRedirect(t *testing.T) {
	publicLookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	tests := []struct {
		name   string
		client httpClient
		match  string
	}{
		{
			name: "oversized",
			client: clientFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 33)))}, nil
			}),
			match: "response limit",
		},
		{
			name: "redirect",
			client: clientFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusFound, Body: io.NopCloser(strings.NewReader(""))}, nil
			}),
			match: "HTTP status 302",
		},
		{
			name: "network timeout",
			client: clientFunc(func(*http.Request) (*http.Response, error) {
				return nil, context.DeadlineExceeded
			}),
			match: "fetch DID log",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := testResolver(test.client, publicLookup)
			did := "did:webvh:Qmetio9KXzDkPXDpSQVyXSTcPVvj5ysHgMZt7y5ffRNDzD:persona.example"
			_, err := resolver.ResolveAuthenticationKey(context.Background(), did, did+"#key-0")
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("error = %v, want match %q", err, test.match)
			}
		})
	}
}

func TestNewResolverDisablesRedirectsAndProxy(t *testing.T) {
	resolver := NewResolver(time.Second)
	client := resolver.client.(*http.Client)
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect() error = %v", err)
	}
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("resolver transport unexpectedly honors an HTTP proxy")
	}
}
