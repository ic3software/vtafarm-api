package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

const baseURL = "https://api.cloudflare.com/client/v4"

type Client struct {
	apiToken string
	zoneID   string
	http     *http.Client
}

type DNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

type createRecordRequest struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type apiResponse[T any] struct {
	Success bool      `json:"success"`
	Errors  []apiError `json:"errors"`
	Result  T         `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func New(apiToken, zoneID string) *Client {
	return &Client{
		apiToken: apiToken,
		zoneID:   zoneID,
		http:     &http.Client{},
	}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")
	return c.http.Do(req)
}

// VerifyZone checks that the API token can read the configured zone.
func (c *Client) VerifyZone(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/zones/%s", baseURL, c.zoneID), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result apiResponse[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	return cfError(result.Success, result.Errors)
}

// CreateARecord creates a proxied A record and returns its Cloudflare record ID.
func (c *Client) CreateARecord(ctx context.Context, name, ip string) (string, error) {
	body, err := json.Marshal(createRecordRequest{
		Type:    "A",
		Name:    name,
		Content: ip,
		TTL:     1, // 1 = automatic
		Proxied: true,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/zones/%s/dns_records", baseURL, c.zoneID),
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result apiResponse[DNSRecord]
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if err := cfError(result.Success, result.Errors); err != nil {
		return "", err
	}
	return result.Result.ID, nil
}

// DeleteRecord removes the DNS record with the given Cloudflare record ID.
func (c *Client) DeleteRecord(ctx context.Context, recordID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/zones/%s/dns_records/%s", baseURL, c.zoneID, recordID), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result apiResponse[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	return cfError(result.Success, result.Errors)
}

func cfError(success bool, errs []apiError) error {
	if success {
		return nil
	}
	if len(errs) > 0 {
		return fmt.Errorf("cloudflare: %s (code %d)", errs[0].Message, errs[0].Code)
	}
	return errors.New("cloudflare: unknown error")
}
