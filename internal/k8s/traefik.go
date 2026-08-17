package k8s

import (
	"context"
	"fmt"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Everything this API knows about the ingress controller lives in this file.
//
// Two things that were per-Ingress annotations under ingress-nginx are now
// entrypoint-level settings on the controller itself
// (k8s/tls/rke2-traefik-config.yaml): the HTTP→HTTPS redirect, and the wildcard
// certificate every managed and platform hostname is served with. That is why
// the Ingresses this package creates carry no annotations at all beyond the
// middleware reference below — there is nothing left for them to say.

// IngressClass is the ingressClassName on every Ingress this API creates.
// Hardcoded rather than configurable: an environment that ran a different
// controller would also need different entrypoint settings, a different default
// certificate and a different ACME solver class, so one env var could never
// carry the switch on its own.
const IngressClass = "traefik"

// StripForwardedHostMiddleware names the per-namespace Middleware that removes
// X-Forwarded-Host from requests on their way to the dids daemon.
//
// The daemon resolves a request's intended host from Host unless the direct TCP
// peer is inside its trusted-CIDR set, and warn-logs every request that claims a
// forwarded host from outside it. Traefik sets X-Forwarded-Host on every proxied
// request, so without this the daemon logs one warning per request, forever.
//
// Removing the header is the right half of that trade rather than trusting the
// ingress: pod networking is flat, so a CIDR wide enough to cover the ingress
// also lets any other pod in the cluster dictate the request host, and the whole
// point of the daemon's gate is that a spoofed X-Forwarded-Host cannot make a
// resolution answer for somebody else's domain. Stripping keeps Host-based
// routing — which is what everything already runs on — and grants nobody
// anything.
//
// Note this is only possible because Traefik writes the X-Forwarded-* headers at
// the entrypoint, before middlewares run. Under ingress-nginx the equivalent
// directive is emitted by the controller's own template ahead of any snippet,
// and nginx sends both values rather than letting the later one win, so the
// header could not be removed there at all.
const StripForwardedHostMiddleware = "strip-forwarded-host"

// middlewareGVR reaches Traefik's CRD through the dynamic client, so we never
// import its Go module for one object. Same reasoning as certificateGVR.
var middlewareGVR = schema.GroupVersionResource{
	Group: "traefik.io", Version: "v1alpha1", Resource: "middlewares",
}

// MiddlewareRef renders the value Traefik's Kubernetes Ingress provider expects
// in the router.middlewares annotation: <namespace>-<name>@kubernetescrd.
//
// The namespace is part of the reference because a Middleware is namespaced and
// cross-namespace references are refused unless the controller was started with
// allowCrossNamespace — which it is not. Hence one Middleware per user
// namespace rather than one shared object.
func MiddlewareRef(ns, name string) string {
	return fmt.Sprintf("%s-%s@kubernetescrd", ns, name)
}

// EnsureStripForwardedHostMiddleware creates the Middleware in ns. Idempotent —
// AlreadyExists is ignored, matching the other Create* helpers.
//
// An empty header value is Traefik's spelling of "delete this header", not "send
// it empty".
func (c *Client) EnsureStripForwardedHostMiddleware(ctx context.Context, ns string) error {
	mw := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "Middleware",
		"metadata": map[string]any{
			"name":      StripForwardedHostMiddleware,
			"namespace": ns,
		},
		"spec": map[string]any{
			"headers": map[string]any{
				"customRequestHeaders": map[string]any{
					"X-Forwarded-Host": "",
				},
			},
		},
	}}

	_, err := c.dyn.Resource(middlewareGVR).Namespace(ns).Create(ctx, mw, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create middleware %s: %w", StripForwardedHostMiddleware, err)
	}
	return nil
}
