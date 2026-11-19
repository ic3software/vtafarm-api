package logquery

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultBatchSize = 5000

type Client struct {
	baseURL   string
	http      *http.Client
	batchSize int
}

type Entry struct {
	Timestamp time.Time         `json:"timestamp"`
	Labels    map[string]string `json:"labels"`
	Line      string            `json:"line"`
	nanos     int64
}

type apiResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string   `json:"stream"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		http:      &http.Client{Timeout: timeout},
		batchSize: defaultBatchSize,
	}
}

// WriteRange writes every matching entry in chronological order using Loki's
// human-readable convention: timestamp, labels, then the original log line.
// Loki caps each range response, so the query advances in nanosecond pages.
func (c *Client) WriteRange(ctx context.Context, query string, start, end time.Time, dst io.Writer) (int, error) {
	if c == nil {
		return 0, fmt.Errorf("logging backend is not configured")
	}
	writer := bufio.NewWriter(dst)
	cursor := start.UnixNano()
	endNanos := end.UnixNano()
	total := 0

	for cursor < endNanos {
		entries, err := c.rangePage(ctx, query, cursor, endNanos)
		if err != nil {
			return total, err
		}
		if len(entries) == 0 {
			break
		}
		for _, entry := range entries {
			if err := writeEntry(writer, entry); err != nil {
				return total, fmt.Errorf("write log export: %w", err)
			}
			total++
		}
		if len(entries) < c.batchSize {
			break
		}
		last := entries[len(entries)-1].nanos
		if last < cursor || last == int64(^uint64(0)>>1) {
			return total, fmt.Errorf("logging backend returned an invalid page cursor")
		}
		cursor = last + 1
	}
	if err := writer.Flush(); err != nil {
		return total, fmt.Errorf("flush log export: %w", err)
	}
	return total, nil
}

func writeEntry(dst io.Writer, entry Entry) error {
	labels := make([]string, 0, len(entry.Labels))
	for name, value := range entry.Labels {
		labels = append(labels, name+"="+strconv.Quote(value))
	}
	sort.Strings(labels)

	line := strings.TrimRight(entry.Line, "\r\n")
	if len(labels) == 0 {
		_, err := fmt.Fprintf(dst, "%s %s\n", entry.Timestamp.Format(time.RFC3339Nano), line)
		return err
	}
	_, err := fmt.Fprintf(dst, "%s {%s} %s\n", entry.Timestamp.Format(time.RFC3339Nano), strings.Join(labels, ", "), line)
	return err
}

func (c *Client) rangePage(ctx context.Context, query string, start, end int64) ([]Entry, error) {
	values := url.Values{
		"query":     {query},
		"start":     {strconv.FormatInt(start, 10)},
		"end":       {strconv.FormatInt(end, 10)},
		"direction": {"forward"},
		"limit":     {strconv.Itoa(c.batchSize)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/loki/api/v1/query_range", strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query logging backend: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read logging response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("logging backend returned %s", resp.Status)
	}
	var result apiResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode logging response: %w", err)
	}
	if result.Status != "success" {
		return nil, fmt.Errorf("logging query failed: %s", result.Error)
	}
	if result.Data.ResultType != "streams" {
		return nil, fmt.Errorf("logging backend returned %q instead of streams", result.Data.ResultType)
	}

	entries := make([]Entry, 0)
	for _, stream := range result.Data.Result {
		for _, raw := range stream.Values {
			if len(raw) != 2 {
				continue
			}
			var timestamp, line string
			if json.Unmarshal(raw[0], &timestamp) != nil || json.Unmarshal(raw[1], &line) != nil {
				continue
			}
			nanos, err := strconv.ParseInt(timestamp, 10, 64)
			if err != nil {
				continue
			}
			entries = append(entries, Entry{
				Timestamp: time.Unix(0, nanos).UTC(),
				Labels:    stream.Stream,
				Line:      line,
				nanos:     nanos,
			})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].nanos != entries[j].nanos {
			return entries[i].nanos < entries[j].nanos
		}
		return entries[i].Labels["pod"] < entries[j].Labels["pod"]
	})
	return entries, nil
}
