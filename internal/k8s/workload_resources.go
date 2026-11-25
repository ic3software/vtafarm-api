package k8s

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

const (
	ComponentVTA           = "vta"
	ComponentMediator      = "mediator"
	ComponentDids          = "dids"
	ComponentVTC           = "vtc"
	ComponentTrustRegistry = "trust-registry"
)

// ResourceProfile is the resource shape used when a component is created or
// updated.
type ResourceProfile struct {
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

// DefaultResourceProfile supplies factory defaults before any admin overrides.
func DefaultResourceProfile(component string) (ResourceProfile, bool) {
	switch component {
	case ComponentVTA:
		return ResourceProfile{CPURequest: "10m", CPULimit: "100m", MemoryRequest: "16Mi", MemoryLimit: "64Mi"}, true
	case ComponentMediator:
		return ResourceProfile{CPURequest: "50m", CPULimit: "250m", MemoryRequest: "128Mi", MemoryLimit: "256Mi"}, true
	case ComponentDids:
		return ResourceProfile{CPURequest: "10m", CPULimit: "100m", MemoryRequest: "64Mi", MemoryLimit: "128Mi"}, true
	case ComponentVTC:
		return ResourceProfile{CPURequest: "10m", CPULimit: "100m", MemoryRequest: "64Mi", MemoryLimit: "256Mi"}, true
	case ComponentTrustRegistry:
		return ResourceProfile{CPURequest: "10m", CPULimit: "100m", MemoryRequest: "64Mi", MemoryLimit: "256Mi"}, true
	default:
		return ResourceProfile{}, false
	}
}

func DefaultResourceRequirements(component string) corev1.ResourceRequirements {
	profile, ok := DefaultResourceProfile(component)
	if !ok {
		panic("unknown component resource profile: " + component)
	}
	return ComponentResourcesWithCPULimit(profile.CPURequest, profile.CPULimit, profile.MemoryRequest, profile.MemoryLimit)
}

func ValidateCPUResources(cpuRequest, cpuLimit string) error {
	request, err := resource.ParseQuantity(strings.TrimSpace(cpuRequest))
	if err != nil || request.Sign() <= 0 {
		return fmt.Errorf("cpu_request must be a positive Kubernetes quantity")
	}
	limit, err := resource.ParseQuantity(strings.TrimSpace(cpuLimit))
	if err != nil || limit.Sign() <= 0 {
		return fmt.Errorf("cpu_limit must be a positive Kubernetes quantity")
	}
	if request.Cmp(limit) > 0 {
		return fmt.Errorf("cpu_request must not exceed cpu_limit")
	}
	return nil
}

// ValidateMemoryResources accepts Kubernetes quantities and enforces the
// relationship Kubernetes requires for a useful workload configuration.
func ValidateMemoryResources(memoryRequest, memoryLimit string) error {
	request, err := resource.ParseQuantity(strings.TrimSpace(memoryRequest))
	if err != nil || request.Sign() <= 0 {
		return fmt.Errorf("memory_request must be a positive Kubernetes quantity")
	}
	limit, err := resource.ParseQuantity(strings.TrimSpace(memoryLimit))
	if err != nil || limit.Sign() <= 0 {
		return fmt.Errorf("memory_limit must be a positive Kubernetes quantity")
	}
	if request.Cmp(limit) > 0 {
		return fmt.Errorf("memory_request must not exceed memory_limit")
	}
	return nil
}

// DeploymentResourceProfile reads the observed resources from the Deployment
// template rather than from an ephemeral Pod.
func (c *Client) DeploymentResourceProfile(ctx context.Context, namespace, name string) (ResourceProfile, error) {
	deploy, err := c.kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ResourceProfile{}, fmt.Errorf("get deployment %s: %w", name, err)
	}
	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return ResourceProfile{}, fmt.Errorf("deployment %s has no containers", name)
	}
	resources := deploy.Spec.Template.Spec.Containers[0].Resources
	return ResourceProfile{
		CPURequest:    quantityString(resources.Requests, corev1.ResourceCPU),
		CPULimit:      quantityString(resources.Limits, corev1.ResourceCPU),
		MemoryRequest: quantityString(resources.Requests, corev1.ResourceMemory),
		MemoryLimit:   quantityString(resources.Limits, corev1.ResourceMemory),
	}, nil
}

func quantityString(list corev1.ResourceList, name corev1.ResourceName) string {
	if value, ok := list[name]; ok {
		return value.String()
	}
	return ""
}

// DryRunDeploymentMemoryUpdate asks the API server to validate the exact
// Deployment update without persisting it.
func (c *Client) DryRunDeploymentMemoryUpdate(ctx context.Context, namespace, name, memoryRequest, memoryLimit string) error {
	return c.updateDeploymentMemory(ctx, namespace, name, memoryRequest, memoryLimit, true)
}

// UpdateDeploymentMemory applies the memory desired state and uses Recreate:
// these workloads mount RWO PVCs, so a surge pod can otherwise block waiting
// for the old pod to release the volume.
func (c *Client) UpdateDeploymentMemory(ctx context.Context, namespace, name, memoryRequest, memoryLimit string) error {
	return c.updateDeploymentMemory(ctx, namespace, name, memoryRequest, memoryLimit, false)
}

func (c *Client) DryRunDeploymentResourceUpdate(ctx context.Context, namespace, name string, profile ResourceProfile) error {
	return c.updateDeploymentResources(ctx, namespace, name, profile, true)
}

func (c *Client) UpdateDeploymentResources(ctx context.Context, namespace, name string, profile ResourceProfile) error {
	return c.updateDeploymentResources(ctx, namespace, name, profile, false)
}

func (c *Client) updateDeploymentResources(ctx context.Context, namespace, name string, profile ResourceProfile, dryRun bool) error {
	update := func() error {
		deploy, err := c.kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get deployment %s: %w", name, err)
		}
		if len(deploy.Spec.Template.Spec.Containers) == 0 {
			return fmt.Errorf("deployment %s has no containers", name)
		}
		deploy.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		if err := applyResourceProfile(&deploy.Spec.Template.Spec.Containers[0], profile); err != nil {
			return err
		}

		opts := metav1.UpdateOptions{}
		if dryRun {
			opts.DryRun = []string{metav1.DryRunAll}
		}
		if _, err := c.kube.AppsV1().Deployments(namespace).Update(ctx, deploy, opts); err != nil {
			return fmt.Errorf("update deployment %s: %w", name, err)
		}
		return nil
	}
	if dryRun {
		return update()
	}
	return retry.RetryOnConflict(retry.DefaultRetry, update)
}

func applyResourceProfile(container *corev1.Container, profile ResourceProfile) error {
	if err := ValidateMemoryResources(profile.MemoryRequest, profile.MemoryLimit); err != nil {
		return err
	}
	if profile.CPULimit != "" {
		if err := ValidateCPUResources(profile.CPURequest, profile.CPULimit); err != nil {
			return err
		}
	} else {
		request, err := resource.ParseQuantity(strings.TrimSpace(profile.CPURequest))
		if err != nil || request.Sign() <= 0 {
			return fmt.Errorf("cpu_request must be a positive Kubernetes quantity")
		}
	}
	cpuRequest := resource.MustParse(strings.TrimSpace(profile.CPURequest))
	memoryRequest := resource.MustParse(strings.TrimSpace(profile.MemoryRequest))
	memoryLimit := resource.MustParse(strings.TrimSpace(profile.MemoryLimit))

	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}
	container.Resources.Requests[corev1.ResourceCPU] = cpuRequest
	container.Resources.Requests[corev1.ResourceMemory] = memoryRequest
	container.Resources.Limits[corev1.ResourceMemory] = memoryLimit
	if profile.CPULimit == "" {
		delete(container.Resources.Limits, corev1.ResourceCPU)
	} else {
		container.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(strings.TrimSpace(profile.CPULimit))
	}
	return nil
}

func (c *Client) updateDeploymentMemory(ctx context.Context, namespace, name, memoryRequest, memoryLimit string, dryRun bool) error {
	if err := ValidateMemoryResources(memoryRequest, memoryLimit); err != nil {
		return err
	}
	request := resource.MustParse(strings.TrimSpace(memoryRequest))
	limit := resource.MustParse(strings.TrimSpace(memoryLimit))

	update := func() error {
		deploy, err := c.kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get deployment %s: %w", name, err)
		}
		if len(deploy.Spec.Template.Spec.Containers) == 0 {
			return fmt.Errorf("deployment %s has no containers", name)
		}
		deploy.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		container := &deploy.Spec.Template.Spec.Containers[0]
		if container.Resources.Requests == nil {
			container.Resources.Requests = corev1.ResourceList{}
		}
		if container.Resources.Limits == nil {
			container.Resources.Limits = corev1.ResourceList{}
		}
		container.Resources.Requests[corev1.ResourceMemory] = request
		container.Resources.Limits[corev1.ResourceMemory] = limit

		opts := metav1.UpdateOptions{}
		if dryRun {
			opts.DryRun = []string{metav1.DryRunAll}
		}
		if _, err := c.kube.AppsV1().Deployments(namespace).Update(ctx, deploy, opts); err != nil {
			return fmt.Errorf("update deployment %s: %w", name, err)
		}
		return nil
	}
	if dryRun {
		return update()
	}
	return retry.RetryOnConflict(retry.DefaultRetry, update)
}

// ValidateMemorySettings applies the admin editing bounds without preventing
// rollback to an older Deployment whose resources fall outside those bounds.
func ValidateMemorySettings(request, limit string) error {
	if err := ValidateMemoryResources(request, limit); err != nil {
		return err
	}
	min, max := resource.MustParse("16Mi"), resource.MustParse("1Gi")
	for _, value := range []string{request, limit} {
		q, _ := resource.ParseQuantity(strings.TrimSpace(value))
		if q.Cmp(min) < 0 || q.Cmp(max) > 0 {
			return fmt.Errorf("memory request and limit must be between 16Mi and 1Gi")
		}
	}
	return nil
}

func ValidateCPUSettings(request, limit string) error {
	if err := ValidateCPUResources(request, limit); err != nil {
		return err
	}
	min, max := resource.MustParse("10m"), resource.MustParse("2")
	for _, value := range []string{request, limit} {
		q, _ := resource.ParseQuantity(strings.TrimSpace(value))
		if q.Cmp(min) < 0 || q.Cmp(max) > 0 {
			return fmt.Errorf("CPU request and limit must be between 10m and 2 cores")
		}
	}
	return nil
}
