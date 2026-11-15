package metrics

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ic3software/vtafarm-api/internal/model"
)

const (
	FreeRetentionDays    = 7
	PremiumRetentionDays = 30
)

type Entitlement struct {
	Plan          string `json:"plan"`
	RetentionDays int    `json:"retention_days"`
}

func EntitlementFor(user model.User) Entitlement {
	if user.FullstackAccess {
		return Entitlement{Plan: "premium", RetentionDays: PremiumRetentionDays}
	}
	return Entitlement{Plan: "free", RetentionDays: FreeRetentionDays}
}

func AvailableComponents(mode string, trustRegistryEnabled bool) []string {
	components := []string{"vta"}
	if mode != "full_stack" {
		return components
	}
	components = append(components, "mediator", "dids", "vtc")
	if trustRegistryEnabled {
		components = append(components, "trust-registry")
	}
	return components
}

type Target struct {
	Scope     string `json:"scope"`
	Component string `json:"component,omitempty"`
	Pod       string `json:"pod,omitempty"`
}

var podNamePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?$`)

func ValidateTarget(target Target, mode string, trustRegistryEnabled bool, currentPods []string) error {
	components := make(map[string]bool)
	for _, component := range AvailableComponents(mode, trustRegistryEnabled) {
		components[component] = true
	}
	switch target.Scope {
	case "stack":
		if mode != "full_stack" {
			return fmt.Errorf("stack scope is only available for full stack sessions")
		}
	case "component":
		if !components[target.Component] {
			return fmt.Errorf("component is not available for this session")
		}
	case "pod":
		if !podNamePattern.MatchString(target.Pod) {
			return fmt.Errorf("invalid pod")
		}
		found := false
		for _, pod := range currentPods {
			found = found || pod == target.Pod
		}
		if !found {
			return fmt.Errorf("pod is not part of this session")
		}
	default:
		return fmt.Errorf("scope must be stack, component, or pod")
	}
	return nil
}

func Selector(sessionID uint, target Target) string {
	parts := []string{fmt.Sprintf(`label_session_id=%q`, fmt.Sprint(sessionID))}
	switch target.Scope {
	case "component":
		parts = append(parts, fmt.Sprintf(`label_app=%q`, componentLabel(target.Component)))
	case "pod":
		parts = append(parts, fmt.Sprintf(`pod=%q`, target.Pod))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func componentLabel(component string) string {
	if component == "vta" {
		return `vta|fs-vta`
	}
	return "fs-" + component
}

type Definition struct {
	Name   string
	Record string
}

var HistoricalDefinitions = []Definition{
	{Name: "cpu_usage_cores", Record: "vtafarm:pod_cpu_usage_cores:rate5m"},
	{Name: "memory_working_set_bytes", Record: "vtafarm:pod_memory_working_set_bytes"},
	{Name: "cpu_requested_cores", Record: "vtafarm:pod_cpu_requested_cores"},
	{Name: "cpu_limit_cores", Record: "vtafarm:pod_cpu_limit_cores"},
	{Name: "memory_requested_bytes", Record: "vtafarm:pod_memory_requested_bytes"},
	{Name: "memory_limit_bytes", Record: "vtafarm:pod_memory_limit_bytes"},
	{Name: "network_receive_bytes_per_second", Record: "vtafarm:pod_network_receive_bytes_per_second:rate5m"},
	{Name: "network_transmit_bytes_per_second", Record: "vtafarm:pod_network_transmit_bytes_per_second:rate5m"},
	{Name: "persistent_volume_used_bytes", Record: "vtafarm:pod_persistent_volume_used_bytes"},
	{Name: "persistent_volume_capacity_bytes", Record: "vtafarm:pod_persistent_volume_capacity_bytes"},
	{Name: "pod_ready", Record: "vtafarm:pod_ready"},
	{Name: "container_restarts_total", Record: "vtafarm:pod_container_restarts_total"},
}

func Query(def Definition, sessionID uint, target Target) string {
	selector := Selector(sessionID, target)
	if target.Scope == "component" && target.Component == "vta" {
		selector = strings.Replace(selector, `label_app="vta|fs-vta"`, `label_app=~"vta|fs-vta"`, 1)
	}
	return fmt.Sprintf("sum by (pod) (%s%s)", def.Record, selector)
}

// RangeQuery rolls dense samples into the resolution selected for the view.
// Gauges and rates use a bucket average; configured ceilings and cumulative
// values use the bucket maximum so reference lines remain exact.
func RangeQuery(def Definition, sessionID uint, target Target, window time.Duration) string {
	selector := Selector(sessionID, target)
	if target.Scope == "component" && target.Component == "vta" {
		selector = strings.Replace(selector, `label_app="vta|fs-vta"`, `label_app=~"vta|fs-vta"`, 1)
	}
	function := "avg_over_time"
	switch def.Name {
	case "cpu_requested_cores", "cpu_limit_cores", "memory_requested_bytes", "memory_limit_bytes",
		"persistent_volume_used_bytes", "persistent_volume_capacity_bytes", "container_restarts_total":
		function = "max_over_time"
	}
	return fmt.Sprintf("sum by (pod) (%s(%s%s[%ds]))", function, def.Record, selector, int64(window.Seconds()))
}

func LiveDefinitions() []Definition {
	definitions := append([]Definition(nil), HistoricalDefinitions...)
	definitions[0].Record = "vtafarm:pod_cpu_usage_cores:rate1m"
	return definitions
}

func SummaryDefinitions() []Definition {
	wanted := map[string]bool{
		"cpu_usage_cores": true, "memory_working_set_bytes": true,
		"persistent_volume_used_bytes": true, "persistent_volume_capacity_bytes": true,
		"container_restarts_total": true,
	}
	definitions := LiveDefinitions()
	result := make([]Definition, 0, len(wanted))
	for _, definition := range definitions {
		if wanted[definition.Name] {
			result = append(result, definition)
		}
	}
	return result
}

func Preset(value string, now time.Time, maxPoints int) (time.Time, time.Time, time.Duration, error) {
	type preset struct {
		duration time.Duration
		step     time.Duration
	}
	presets := map[string]preset{
		"1h":  {duration: time.Hour, step: time.Minute},
		"6h":  {duration: 6 * time.Hour, step: time.Minute},
		"24h": {duration: 24 * time.Hour, step: 2 * time.Minute},
		"7d":  {duration: 7 * 24 * time.Hour, step: 10 * time.Minute},
		"30d": {duration: 30 * 24 * time.Hour, step: time.Hour},
	}
	selected, ok := presets[value]
	if !ok {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("range must be 1h, 6h, 24h, 7d, or 30d")
	}
	step := selected.step
	if maxPoints > 0 && selected.duration/step > time.Duration(maxPoints) {
		step = selected.duration / time.Duration(maxPoints)
		step = ((step + time.Second - 1) / time.Second) * time.Second
	}
	return now.Add(-selected.duration), now, step, nil
}

func PodNames(pods map[string]string) []string {
	result := make([]string, 0, len(pods))
	for name := range pods {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}
