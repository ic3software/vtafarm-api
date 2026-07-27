package k8s

import (
	"context"
	"fmt"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TLS for custom domains: one cert-manager Certificate per session, covering
// all four of its hostnames.
//
// Managed and platform sessions never reach this file — their names are under
// our own zone, which the *.{CLUSTER_DOMAIN} wildcard already covers, so they
// consume no ACME quota at all.

// certificateGVR reaches cert-manager through the dynamic client, so we never
// import its Go module for three fields.
var certificateGVR = schema.GroupVersionResource{
	Group: "cert-manager.io", Version: "v1", Resource: "certificates",
}

// CreateSessionCert creates one Certificate covering every hostname the session
// serves. Idempotent — AlreadyExists is ignored, matching the other Create*
// helpers here.
//
// One object rather than four is what cert-manager's ingress-shim would have
// produced from an annotation. It does **not** relax the rate limit that
// actually binds (five certificates per identical name set per week, either
// way). What it buys is one Secret instead of four, one readiness condition to
// poll instead of four, and a failure you diagnose with a single lookup.
//
// The four Ingresses must reference this Secret **without** a
// `cert-manager.io/cluster-issuer` annotation. If that annotation is left on
// them while this object also exists, ingress-shim creates a second Certificate
// pointing at the same secretName, the two overwrite each other, and issuance
// loops — burning ACME quota continuously rather than once.
func (c *Client) CreateSessionCert(ctx context.Context, ns, name, issuer string, hosts []string) error {
	dnsNames := make([]any, len(hosts))
	for i, h := range hosts {
		dnsNames[i] = h
	}

	cert := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]any{
			"secretName": name,
			"issuerRef": map[string]any{
				"name": issuer,
				"kind": "ClusterIssuer",
			},
			"dnsNames": dnsNames,
		},
	}}

	_, err := c.dyn.Resource(certificateGVR).Namespace(ns).Create(ctx, cert, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create certificate %s: %w", name, err)
	}
	return nil
}

// CertReady reports status.conditions[type=Ready]. The message comes back too
// because it is the only actionable thing we can show when issuance times out —
// it usually names a CAA record or a DNS change made after verification.
func (c *Client) CertReady(ctx context.Context, ns, name string) (ready bool, message string, err error) {
	obj, err := c.dyn.Resource(certificateGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, "", fmt.Errorf("get certificate %s: %w", name, err)
	}

	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		// No conditions yet — cert-manager hasn't picked it up. Not an error;
		// the caller is polling.
		return false, "", nil
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if cond["type"] != "Ready" {
			continue
		}
		msg, _ := cond["message"].(string)
		return cond["status"] == "True", msg, nil
	}
	return false, "", nil
}

// DeleteSessionCert removes the Certificate.
//
// **The Secret is deliberately left behind.** Recreating a session on the same
// domain asks for a certificate covering the exact same four names, which is
// precisely what Let's Encrypt's "5 per identical set of identifiers per 7
// days" limit counts — and that limit cannot be raised. If the user's namespace
// survives (they still have other sessions), the next session names the same
// Secret (CustomTLSSecret is keyed by domain), finds a valid certificate
// already in it, and cert-manager adopts it at no ACME cost. Namespace deletion
// cleans it up when their last session goes — or DeleteDomainCert does, when
// the domain itself is detached.
func (c *Client) DeleteSessionCert(ctx context.Context, ns, name string) error {
	err := c.dyn.Resource(certificateGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("delete certificate %s: %w", name, err)
	}
	return nil
}

// DeleteDomainCert removes the Certificate **and** its Secret.
//
// This is the counterpart to DeleteSessionCert, for when the domain itself is
// detached rather than the session on it. Keeping the certificate then would be
// wrong twice over: it is key material for names the account no longer claims —
// and a domain can be attached by somebody else afterwards — and it is
// unreachable anyway, since re-attaching mints a new domains row whose id names
// a different Secret. Deleting it also means a re-attach genuinely starts over,
// which is what someone who detached to fix something expects.
//
// Errors are returned per resource but neither aborts the other: a leftover of
// either kind is worth reporting and worth not compounding.
func (c *Client) DeleteDomainCert(ctx context.Context, ns, name string) error {
	certErr := c.dyn.Resource(certificateGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if certErr != nil && k8serrors.IsNotFound(certErr) {
		certErr = nil
	}

	secretErr := c.kube.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if secretErr != nil && k8serrors.IsNotFound(secretErr) {
		secretErr = nil
	}

	switch {
	case certErr != nil && secretErr != nil:
		return fmt.Errorf("delete certificate %s: %w (and its secret: %v)", name, certErr, secretErr)
	case certErr != nil:
		return fmt.Errorf("delete certificate %s: %w", name, certErr)
	case secretErr != nil:
		return fmt.Errorf("delete tls secret %s: %w", name, secretErr)
	}
	return nil
}
