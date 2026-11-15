package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestEntitlementFor(t *testing.T) {
	free := EntitlementFor(model.User{})
	if free.Plan != "free" || free.RetentionDays != 7 {
		t.Fatalf("free entitlement = %#v", free)
	}
	premium := EntitlementFor(model.User{FullstackAccess: true})
	if premium.Plan != "premium" || premium.RetentionDays != 30 {
		t.Fatalf("premium entitlement = %#v", premium)
	}
}

func TestValidateTarget(t *testing.T) {
	if err := ValidateTarget(Target{Scope: "stack"}, "vta_only", false, nil); err == nil {
		t.Fatal("vta_only accepted stack scope")
	}
	if err := ValidateTarget(Target{Scope: "component", Component: "mediator"}, "vta_only", false, nil); err == nil {
		t.Fatal("vta_only accepted mediator")
	}
	if err := ValidateTarget(Target{Scope: "component", Component: "trust-registry"}, "full_stack", false, nil); err == nil {
		t.Fatal("full_stack without trust registry accepted trust-registry")
	}
	if err := ValidateTarget(Target{Scope: "component", Component: "trust-registry"}, "full_stack", true, nil); err != nil {
		t.Fatalf("enabled trust registry rejected: %v", err)
	}
	if err := ValidateTarget(Target{Scope: "pod", Pod: "vta-42-abc"}, "full_stack", false, []string{"vta-42-abc"}); err != nil {
		t.Fatalf("current pod rejected: %v", err)
	}
	if err := ValidateTarget(Target{Scope: "pod", Pod: `vta-42-abc"}`}, "full_stack", false, []string{`vta-42-abc"}`}); err == nil {
		t.Fatal("unsafe pod name accepted")
	}
}

func TestAvailableComponents(t *testing.T) {
	withoutRegistry := AvailableComponents("full_stack", false)
	if strings.Join(withoutRegistry, ",") != "vta,mediator,dids,vtc" {
		t.Fatalf("components without registry = %v", withoutRegistry)
	}
	withRegistry := AvailableComponents("full_stack", true)
	if strings.Join(withRegistry, ",") != "vta,mediator,dids,vtc,trust-registry" {
		t.Fatalf("components with registry = %v", withRegistry)
	}
}

func TestQueryUsesOnlyServerSelectors(t *testing.T) {
	query := Query(HistoricalDefinitions[0], 42, Target{Scope: "component", Component: "vta"})
	for _, expected := range []string{`label_session_id="42"`, `label_app=~"vta|fs-vta"`, "sum by (pod)"} {
		if !strings.Contains(query, expected) {
			t.Fatalf("query %q missing %q", query, expected)
		}
	}
}

func TestRangeQueryAggregatesIntoViewBuckets(t *testing.T) {
	target := Target{Scope: "component", Component: "vta"}
	usage := RangeQuery(HistoricalDefinitions[0], 42, target, time.Hour)
	if !strings.Contains(usage, "avg_over_time(") || !strings.Contains(usage, "[3600s]") {
		t.Fatalf("usage query is not hourly average: %q", usage)
	}
	limit := RangeQuery(HistoricalDefinitions[3], 42, target, time.Hour)
	if !strings.Contains(limit, "max_over_time(") {
		t.Fatalf("limit query does not preserve bucket maximum: %q", limit)
	}
}

func TestPresetBoundsPoints(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	start, end, step, err := Preset("30d", now, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if end.Sub(start) != 30*24*time.Hour || step != time.Hour {
		t.Fatalf("unexpected range=%v step=%v", end.Sub(start), step)
	}
	_, _, dayStep, err := Preset("24h", now, 2000)
	if err != nil || dayStep != 2*time.Minute {
		t.Fatalf("24h step=%v err=%v", dayStep, err)
	}
	if _, _, _, err := Preset("90d", now, 2000); err == nil {
		t.Fatal("unsupported range accepted")
	}
}
