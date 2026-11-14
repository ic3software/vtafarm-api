package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientRangeUsesPostAndParsesMatrix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/query_range" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("query") != `up{tenant="42"}` {
			t.Errorf("form = %#v, err=%v", r.Form, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","warnings":["partial"],"data":{"resultType":"matrix","result":[{"metric":{"pod":"vta-42"},"values":[[1700000000,"1.5"],[1700000030,"2"]]}]}}`)
	}))
	defer server.Close()

	client := NewClient(server.URL, time.Second)
	series, warnings, err := client.Range(context.Background(), "ready", `up{tenant="42"}`, time.Unix(1_700_000_000, 0), time.Unix(1_700_000_030, 0), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || len(series[0].Points) != 2 || series[0].Points[1].Value != 2 || len(warnings) != 1 {
		t.Fatalf("series=%#v warnings=%#v", series, warnings)
	}
}

func TestNewClientEmptyURLDisablesMetrics(t *testing.T) {
	if client := NewClient("  ", time.Second); client != nil {
		t.Fatalf("empty URL created client: %#v", client)
	}
}
