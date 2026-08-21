package k8s

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	"github.com/ic3software/vtafarm-api/internal/config"
)

type Client struct {
	kube kubernetes.Interface
	// dyn reaches resources outside the typed clientset — currently the
	// Longhorn CRDs (nodes/settings.longhorn.io) behind the admin dashboard's
	// storage stats.
	dyn             dynamic.Interface
	restCfg         *rest.Config
	namespacePrefix string
}

// NewClient creates a K8s client.
// When running inside a cluster (production), it uses the in-cluster ServiceAccount.
// When running outside (development), it reads the kubeconfig file.
func NewClient(cfg *config.Config) (*Client, error) {
	var restCfg *rest.Config
	var err error

	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		// In-cluster: use the mounted ServiceAccount token
		restCfg, err = rest.InClusterConfig()
	} else {
		// Out-of-cluster: use kubeconfig
		kubeconfig := cfg.K8s.Kubeconfig
		if kubeconfig == "" {
			kubeconfig = os.ExpandEnv("$HOME/.kube/config")
		}
		restCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("build rest config: %w", err)
	}

	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}

	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic.NewForConfig: %w", err)
	}

	return &Client{
		kube:            kube,
		dyn:             dyn,
		restCfg:         restCfg,
		namespacePrefix: cfg.K8s.NamespacePrefix,
	}, nil
}

// UserNamespace returns the isolated K8s namespace for a given user.
// Pattern: {prefix}-{userID}  e.g. vtafarm-user-abc123
func (c *Client) UserNamespace(userID string) string {
	return fmt.Sprintf("%s-%s", c.namespacePrefix, userID)
}

// EnsureUserEnvironment idempotently creates:
//   - Namespace scoped to the user
//   - ServiceAccount "pod-operator" inside the namespace
//   - Role with pod + exec permissions
//   - RoleBinding wiring the SA to the Role
//
// This call is safe to repeat; AlreadyExists errors are ignored.
// For terminal (exec) access, the API server must also hold cluster-level
// permissions to proxy into pods/exec — typically via a ClusterRole bound
// to the API server's own ServiceAccount (managed in the deployment manifest).
func (c *Client) EnsureUserEnvironment(ctx context.Context, userID string) error {
	ns := c.UserNamespace(userID)

	// 1. Namespace
	_, err := c.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Labels: map[string]string{
				"managed-by": "vtafarm",
				"user-id":    userID,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace: %w", err)
	}

	// 2. ServiceAccount
	saName := "pod-operator"
	_, err = c.kube.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: ns},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create service account: %w", err)
	}

	// 2b. ServiceAccount "vta" — the identity the VTA setup/import jobs and
	// Deployment run as. Vault's kubernetes-auth role for this user is bound to
	// this SA name + namespace, so the VTA can read/write only its own seeds.
	_, err = c.kube.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: VtaServiceAccount, Namespace: ns},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create vta service account: %w", err)
	}

	// 3. Role — scoped to this namespace only
	roleName := "pod-manager"
	_, err = c.kube.RbacV1().Roles(ns).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: ns},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods", "pods/log"},
				Verbs:     []string{"get", "list", "watch", "create", "delete"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods/exec"},
				Verbs:     []string{"create"},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create role: %w", err)
	}

	// 4. RoleBinding
	_, err = c.kube.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-manager-binding", Namespace: ns},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: saName, Namespace: ns}},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: roleName},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create role binding: %w", err)
	}

	return nil
}

// CreatePodFromYAML parses a YAML pod spec and creates the pod in the user's namespace.
// The namespace field in the YAML is always overridden to enforce isolation.
func (c *Client) CreatePodFromYAML(ctx context.Context, userID, yamlContent string) (*corev1.Pod, error) {
	var pod corev1.Pod
	if err := yaml.Unmarshal([]byte(yamlContent), &pod); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	pod.Namespace = c.UserNamespace(userID)

	created, err := c.kube.CoreV1().Pods(pod.Namespace).Create(ctx, &pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create pod: %w", err)
	}
	return created, nil
}

// GetPod returns the live state of a pod in the user's namespace.
func (c *Client) GetPod(ctx context.Context, userID, podName string) (*corev1.Pod, error) {
	ns := c.UserNamespace(userID)
	pod, err := c.kube.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod: %w", err)
	}
	return pod, nil
}

// ListPods returns all pods in the user's namespace.
func (c *Client) ListPods(ctx context.Context, userID string) ([]corev1.Pod, error) {
	ns := c.UserNamespace(userID)
	list, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	return list.Items, nil
}

// DeletePod removes a pod from the user's namespace.
func (c *Client) DeletePod(ctx context.Context, userID, podName string) error {
	ns := c.UserNamespace(userID)
	if err := c.kube.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete pod: %w", err)
	}
	return nil
}

// DeleteNamespace removes the user's namespace and everything inside it.
// IsNotFound is treated as success (idempotent).
func (c *Client) DeleteNamespace(ctx context.Context, userID string) error {
	ns := c.UserNamespace(userID)
	err := c.kube.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace: %w", err)
	}
	return nil
}
