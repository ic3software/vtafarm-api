package k8s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

const execOutputLimit = 4 << 20 // 4 MiB

// RunningPod returns a Running pod matching selector and its first container.
// Unlike StreamComponentPodLogs it does not wait for one to appear.
func (c *Client) RunningPod(ctx context.Context, ns, selector string) (pod, container string, err error) {
	pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", "", fmt.Errorf("list pods (%s): %w", selector, err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil || len(p.Spec.Containers) == 0 {
			continue
		}
		return p.Name, p.Spec.Containers[0].Name, nil
	}
	return "", "", fmt.Errorf("no running pod (%s)", selector)
}

// ExecCapture runs cmd in a container and returns its stdout. stderr is kept
// apart so a warning cannot corrupt the captured file.
func (c *Client) ExecCapture(ctx context.Context, ns, pod, container string, cmd []string) (string, error) {
	if c.restCfg == nil {
		return "", fmt.Errorf("exec: no rest config")
	}

	req := c.kube.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.restCfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("exec %s/%s: %w", ns, pod, err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &limitedWriter{w: &stdout, remaining: execOutputLimit},
		Stderr: &limitedWriter{w: &stderr, remaining: 8 << 10},
	})
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("exec %s/%s: %w: %s", ns, pod, err, msg)
		}
		return "", fmt.Errorf("exec %s/%s: %w", ns, pod, err)
	}
	return stdout.String(), nil
}

// limitedWriter truncates past remaining rather than erroring, so an oversized
// stream still yields what fits instead of failing the whole exec.
type limitedWriter struct {
	w         io.Writer
	remaining int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}
	if len(p) > l.remaining {
		if _, err := l.w.Write(p[:l.remaining]); err != nil {
			return 0, err
		}
		l.remaining = 0
		return len(p), nil
	}
	n, err := l.w.Write(p)
	l.remaining -= n
	return n, err
}
