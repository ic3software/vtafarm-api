package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Point struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

type Series struct {
	Metric string            `json:"metric"`
	Labels map[string]string `json:"labels,omitempty"`
	Points []Point           `json:"points"`
}

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: timeout}}
}

type apiResponse struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings"`
	Error    string   `json:"error"`
	Data     struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string   `json:"metric"`
			Value  []json.RawMessage   `json:"value"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (c *Client) Range(ctx context.Context, metric, query string, start, end time.Time, step time.Duration) ([]Series, []string, error) {
	values := url.Values{
		"query": {query}, "start": {formatTime(start)}, "end": {formatTime(end)},
		"step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	response, err := c.do(ctx, "/api/v1/query_range", values)
	if err != nil {
		return nil, nil, err
	}
	series := make([]Series, 0, len(response.Data.Result))
	for _, result := range response.Data.Result {
		points := make([]Point, 0, len(result.Values))
		for _, raw := range result.Values {
			point, err := parsePoint(raw)
			if err == nil {
				points = append(points, point)
			}
		}
		series = append(series, Series{Metric: metric, Labels: result.Metric, Points: points})
	}
	return series, response.Warnings, nil
}

func (c *Client) Instant(ctx context.Context, metric, query string, at time.Time) ([]Series, []string, error) {
	response, err := c.do(ctx, "/api/v1/query", url.Values{"query": {query}, "time": {formatTime(at)}})
	if err != nil {
		return nil, nil, err
	}
	series := make([]Series, 0, len(response.Data.Result))
	for _, result := range response.Data.Result {
		point, err := parsePoint(result.Value)
		if err != nil {
			continue
		}
		series = append(series, Series{Metric: metric, Labels: result.Metric, Points: []Point{point}})
	}
	return series, response.Warnings, nil
}

func (c *Client) do(ctx context.Context, path string, values url.Values) (*apiResponse, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("metrics backend is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query metrics backend: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("metrics backend returned %s", resp.Status)
	}
	var result apiResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode metrics response: %w", err)
	}
	if result.Status != "success" {
		return nil, fmt.Errorf("metrics query failed: %s", result.Error)
	}
	return &result, nil
}

func parsePoint(raw []json.RawMessage) (Point, error) {
	if len(raw) != 2 {
		return Point{}, fmt.Errorf("invalid sample")
	}
	var timestamp float64
	var value string
	if err := json.Unmarshal(raw[0], &timestamp); err != nil {
		return Point{}, err
	}
	if err := json.Unmarshal(raw[1], &value); err != nil {
		return Point{}, err
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return Point{Timestamp: int64(timestamp), Value: parsed}, err
}

func formatTime(value time.Time) string {
	return strconv.FormatFloat(float64(value.UnixNano())/1e9, 'f', 3, 64)
}
