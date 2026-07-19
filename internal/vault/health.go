package vault

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Health reports the Vault server's state for the monitor endpoints: "ok",
// "sealed", or "uninitialized", or an error when Vault can't be reached at
// all. It is a standalone function (not a Client method) because it needs no
// AppRole credentials — /v1/sys/health is unauthenticated by design, and the
// monitor must keep working even when the full client failed to initialise.
func Health(ctx context.Context, addr string, skipVerify bool) (string, error) {
	tr := &http.Transport{}
	if skipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: tr}

	// standbyok: a standby node is a healthy Vault for our purposes.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/v1/sys/health?standbyok=true", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach vault: %w", err)
	}
	defer resp.Body.Close()

	// sys/health encodes state in the status code (200 ok, 503 sealed, 501
	// uninitialized) but the body carries the same flags — decode those, so an
	// unexpected code with a valid body still yields the right answer.
	var body struct {
		Initialized bool `json:"initialized"`
		Sealed      bool `json:"sealed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("parse vault health (HTTP %d): %w", resp.StatusCode, err)
	}
	switch {
	case !body.Initialized:
		return "uninitialized", nil
	case body.Sealed:
		return "sealed", nil
	default:
		return "ok", nil
	}
}
