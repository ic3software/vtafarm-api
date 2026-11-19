package logquery

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestWriteRangePagesAndSortsEntries(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/loki/api/v1/query_range" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		body := parseForm(t, r)
		if body.Get("direction") != "forward" || body.Get("query") != `{namespace="tenant"}` {
			t.Fatalf("unexpected query form: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		switch requests {
		case 1:
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"pod":"b"},"values":[["11","second"]]},{"stream":{"pod":"a"},"values":[["10","first"]]}]}}`)
		case 2:
			if body.Get("start") != "12" {
				t.Fatalf("second page start = %s, want 12", body.Get("start"))
			}
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"pod":"c"},"values":[["12","third"]]}]}}`)
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, time.Second)
	client.batchSize = 2
	var output bytes.Buffer
	count, err := client.WriteRange(context.Background(), `{namespace="tenant"}`, time.Unix(0, 10), time.Unix(0, 20), &output)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || requests != 2 {
		t.Fatalf("count=%d requests=%d", count, requests)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d", len(lines))
	}
	for i, want := range []string{
		`1970-01-01T00:00:00.00000001Z {pod="a"} first`,
		`1970-01-01T00:00:00.000000011Z {pod="b"} second`,
		`1970-01-01T00:00:00.000000012Z {pod="c"} third`,
	} {
		if lines[i] != want {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want)
		}
	}
}

func TestWriteEntrySortsLabelsAndTrimsTrailingNewline(t *testing.T) {
	var output bytes.Buffer
	err := writeEntry(&output, Entry{
		Timestamp: time.Date(2026, 11, 19, 8, 0, 0, 123, time.UTC),
		Labels:    map[string]string{"pod": "vta-7-a", "app": "vta"},
		Line:      "started\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-11-19T08:00:00.000000123Z {app=\"vta\", pod=\"vta-7-a\"} started\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestNewClientEmptyURLDisablesLoki(t *testing.T) {
	if client := NewClient("   ", time.Second); client != nil {
		t.Fatal("empty URL should keep the Kubernetes log fallback enabled")
	}
}

func parseForm(t *testing.T, r *http.Request) url.Values {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	return r.PostForm
}
