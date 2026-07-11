// Package mailer sends transactional email through the Resend API
// (https://resend.com/docs/api-reference/emails/send-email).
package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const baseURL = "https://api.resend.com"

type Client struct {
	apiKey string
	from   string
	http   *http.Client
}

// New returns a client that sends from the given address, e.g.
// "VTA Farm <noreply@example.com>". The address's domain must be verified in
// the Resend account the API key belongs to.
func New(apiKey, from string) *Client {
	return &Client{
		apiKey: apiKey,
		from:   from,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

type sendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
}

// apiError is Resend's error body, e.g.
// {"statusCode":403,"name":"validation_error","message":"..."}.
type apiError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// Send delivers one HTML email and returns the Resend email id.
func (c *Client) Send(ctx context.Context, to, subject, html string) (string, error) {
	body, err := json.Marshal(sendRequest{
		From:    c.from,
		To:      []string{to},
		Subject: subject,
		HTML:    html,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/emails", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e apiError
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Message != "" {
			return "", fmt.Errorf("resend: %s (%s)", e.Message, e.Name)
		}
		return "", fmt.Errorf("resend: unexpected status %d", resp.StatusCode)
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ID, nil
}
