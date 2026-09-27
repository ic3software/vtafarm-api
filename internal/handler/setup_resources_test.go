package handler

import (
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestValidateResourceBatch(t *testing.T) {
	input := resourceBatchInput{
		SessionIDs: []string{" alpha ", "beta"},
		Resources: []memoryResourceInput{{
			Component: k8s.ComponentVTA, MemoryRequest: "0.125Gi", MemoryLimit: "0.5Gi",
		}},
	}
	if err := validateResourceBatch(&input); err != nil {
		t.Fatal(err)
	}
	if input.SessionIDs[0] != "alpha" || input.Resources[0].MemoryRequest != "128Mi" || input.Resources[0].MemoryLimit != "512Mi" {
		t.Fatalf("input was not normalized: %+v", input)
	}
}

func TestValidateResourceBatchRejectsInvalidInput(t *testing.T) {
	tests := []resourceBatchInput{
		{},
		{SessionIDs: []string{"same", "same"}, Resources: []memoryResourceInput{{Component: "vta", MemoryRequest: "128Mi", MemoryLimit: "512Mi"}}},
		{SessionIDs: []string{"one"}, Resources: []memoryResourceInput{{Component: "unknown", MemoryRequest: "128Mi", MemoryLimit: "512Mi"}}},
		{SessionIDs: []string{"one"}, Resources: []memoryResourceInput{{Component: "vta", MemoryRequest: "1Gi", MemoryLimit: "512Mi"}}},
		{SessionIDs: []string{"one"}, Resources: []memoryResourceInput{{Component: "vta", MemoryRequest: "1Gi", MemoryLimit: "2Gi"}}},
	}
	for i := range tests {
		if err := validateResourceBatch(&tests[i]); err == nil {
			t.Errorf("case %d was accepted: %+v", i, tests[i])
		}
	}
}

func TestSessionWorkloadTargets(t *testing.T) {
	vtaOnly := sessionWorkloadTargets(&model.SetupSession{ID: 7, Mode: model.ModeVtaOnly})
	if len(vtaOnly) != 1 || vtaOnly[0].Component != "vta" || vtaOnly[0].Deployment != "vta-7" {
		t.Fatalf("vta_only targets = %+v", vtaOnly)
	}
	fullStack := sessionWorkloadTargets(&model.SetupSession{ID: 8, Mode: model.ModeFullStack})
	if len(fullStack) != 4 {
		t.Fatalf("full_stack target count = %d", len(fullStack))
	}
	var names []string
	for _, target := range fullStack {
		names = append(names, target.Deployment)
	}
	if got := strings.Join(names, ","); got != "fs-8-vta,fs-8-mediator,fs-8-dids,fs-8-vtc" {
		t.Fatalf("full_stack targets = %s", got)
	}
}
