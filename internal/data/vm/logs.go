package vm

import (
	"context"
	"errors"
	"io"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// The serial console carries guest-visible boot output (kernel, cloud-init).
// virt-launcher's compute container is the fallback when no console sidecar runs.
const (
	guestConsoleContainer = "guest-console-log"
	launcherContainer     = "compute"
)

type podLogSource interface {
	Open(context.Context, string, string, string, uint32, bool) (io.ReadCloser, error)
}
type kubernetesPodLogs struct{ client coreclient.CoreV1Interface }

func (s kubernetesPodLogs) Open(ctx context.Context, namespace, pod, container string, tail uint32, follow bool) (io.ReadCloser, error) {
	lines := int64(tail)
	return s.client.Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, TailLines: &lines, Follow: follow}).Stream(ctx)
}

// ConfigureLogs creates a separate client with no whole-response HTTP timeout:
// opening is bounded by OpenTimeoutSeconds, the body by the stream lifetime.
func (n *Native) ConfigureLogs(config *rest.Config, limits biz.LogLimits) error {
	if config == nil || biz.ValidateLogLimits(limits) != nil {
		return biz.ErrInvalid
	}
	c := rest.CopyConfig(config)
	c.Timeout = 0
	client, e := coreclient.NewForConfig(c)
	if e != nil {
		return e
	}
	n.logSource = kubernetesPodLogs{client: client}
	n.logLimits = limits.Defaults()
	return nil
}

func logContainer(p *unstructured.Unstructured) string {
	var names []string
	for _, field := range []string{"initContainers", "containers"} {
		list, _, _ := unstructured.NestedSlice(p.Object, "spec", field)
		for _, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if name, _ := entry["name"].(string); name != "" {
				names = append(names, name)
			}
		}
	}
	for _, preferred := range []string{guestConsoleContainer, launcherContainer} {
		for _, name := range names {
			if name == preferred {
				return preferred
			}
		}
	}
	if len(names) == 1 {
		return names[0]
	}
	return ""
}

// logPod re-reads the launcher so a replaced or foreign Pod can never be
// streamed under the observed identity.
func (n *Native) logPod(ctx context.Context, v biz.VM, x biz.Observation) (*unstructured.Unstructured, string, error) {
	if x.Launcher == nil || x.VMI == nil {
		return nil, "", biz.ErrLogTarget
	}
	p, e := n.client.Resource(podResource).Namespace(v.Spec.Namespace).Get(ctx, x.Launcher.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(e) {
		return nil, "", biz.ErrChanged
	}
	if e != nil {
		return nil, "", e
	}
	if string(p.GetUID()) != x.Launcher.UID || p.GetDeletionTimestamp() != nil || !matches(p, v) || !owned(p, "VirtualMachineInstance", x.VMI.UID) {
		return nil, "", biz.ErrChanged
	}
	container := logContainer(p)
	if container == "" {
		return nil, "", biz.ErrLogTarget
	}
	return p, container, nil
}

func (n *Native) StreamLogs(ctx context.Context, v biz.VM, x biz.Observation, tail uint32, follow bool, emit func(biz.LogFrame) error) error {
	if n.logSource == nil || tail < 1 || tail > 1000 || emit == nil {
		return biz.ErrLogTarget
	}
	pod, container, e := n.logPod(ctx, v, x)
	if e != nil {
		if errors.Is(e, biz.ErrChanged) {
			return biz.ErrLogTarget
		}
		return e
	}
	limits := n.logLimits.Defaults()
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	opening := time.AfterFunc(time.Duration(limits.OpenTimeoutSeconds)*time.Second, cancel)
	body, e := n.logSource.Open(streamCtx, v.Spec.Namespace, pod.GetName(), container, tail, follow)
	stopped := opening.Stop()
	if e != nil {
		if body != nil {
			_ = body.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !stopped {
			return biz.ErrLogTarget
		}
		return e
	}
	if body == nil {
		return biz.ErrLogTarget
	}
	defer body.Close()
	if !stopped || streamCtx.Err() != nil {
		return biz.ErrLogTarget
	}
	// No body bytes are released until this second GET confirms the same UID.
	if _, current, e := n.logPod(ctx, v, x); e != nil || current != container {
		return biz.ErrLogTarget
	}
	if e = emit(biz.LogFrame{Kind: "READY"}); e != nil {
		return e
	}
	frameBytes := limits.FrameBytes
	if frameBytes > limits.BufferBytes/2 {
		frameBytes = limits.BufferBytes / 2
	}
	if frameBytes < 1 {
		return biz.ErrInvalid
	}
	capacity := limits.BufferBytes/frameBytes - 2
	if capacity > 8 {
		capacity = 8
	}
	if capacity < 1 {
		capacity = 1
	}
	type chunk struct {
		data []byte
		err  error
	}
	chunks := make(chan chunk, capacity)
	go func() {
		for {
			buffer := make([]byte, frameBytes)
			read, e := body.Read(buffer)
			if read > 0 {
				select {
				case chunks <- chunk{data: buffer[:read]}:
				case <-streamCtx.Done():
					return
				}
			}
			if e != nil {
				select {
				case chunks <- chunk{err: e}:
				case <-streamCtx.Done():
				}
				return
			}
		}
	}()
	ticker := time.NewTicker(time.Duration(limits.TargetPollSeconds) * time.Second)
	defer ticker.Stop()
	for {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return emit(biz.LogFrame{Kind: "END", Reason: "TIME_LIMIT"})
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return emit(biz.LogFrame{Kind: "END", Reason: "TIME_LIMIT"})
			}
			return ctx.Err()
		case <-ticker.C:
			if _, current, e := n.logPod(ctx, v, x); e != nil || current != container {
				if errors.Is(e, biz.ErrChanged) {
					return emit(biz.LogFrame{Kind: "END", Reason: "POD_CHANGED"})
				}
				return emit(biz.LogFrame{Kind: "ERROR", Reason: "VM_LOG_TARGET_UNAVAILABLE"})
			}
		case c := <-chunks:
			if len(c.data) > 0 {
				if e = emit(biz.LogFrame{Kind: "DATA", Data: c.data}); e != nil {
					return e
				}
			}
			if c.err != nil {
				if errors.Is(c.err, io.EOF) {
					return emit(biz.LogFrame{Kind: "END", Reason: "EOF"})
				}
				return emit(biz.LogFrame{Kind: "ERROR", Reason: "VM_LOG_UPSTREAM_UNAVAILABLE"})
			}
		}
	}
}
