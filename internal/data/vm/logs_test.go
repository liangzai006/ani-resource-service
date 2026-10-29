package vm

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

const (
	vmLogTenant   = "tenant-logs"
	vmLogPod      = "virt-launcher-vm-logs-abc"
	vmLogPodUID   = "pod-uid"
	vmLogVMIUID   = "vmi-uid"
	vmLogVMUID    = "vm-uid"
	vmLogVMPersID = "11111111-1111-4111-8111-111111111111"
)

type vmLogSource struct {
	container string
	body      io.ReadCloser
	open      func() error
}

func (s *vmLogSource) Open(_ context.Context, _, _, container string, _ uint32, _ bool) (io.ReadCloser, error) {
	s.container = container
	if s.open != nil {
		if e := s.open(); e != nil {
			return nil, e
		}
	}
	return s.body, nil
}

type vmLogReader struct {
	io.Reader
	closed bool
}

func (r *vmLogReader) Close() error { r.closed = true; return nil }

func vmLogFixture(t *testing.T) (*Native, biz.VM, biz.Observation, *fake.FakeDynamicClient, *unstructured.Unstructured) {
	t.Helper()
	v := biz.VM{TenantID: "tenant-uuid", ID: vmLogVMPersID, Spec: biz.Spec{Namespace: vmLogTenant, Digest: "digest"}, Native: biz.ObjectRef{Kind: "VirtualMachine", Namespace: vmLogTenant, Name: "vm-logs", UID: vmLogVMUID}}
	controller := true
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      vmLogPod,
			"namespace": vmLogTenant,
			"uid":       vmLogPodUID,
			"labels":    map[string]any{},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "name": v.Native.Name,
				"uid": vmLogVMIUID, "controller": controller, "blockOwnerDeletion": controller,
			}},
		},
		"spec": map[string]any{
			"initContainers": []any{map[string]any{"name": guestConsoleContainer}},
			"containers":     []any{map[string]any{"name": launcherContainer}},
		},
	}}
	_ = unstructured.SetNestedStringMap(pod.Object, labelVM(v), "metadata", "labels")
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{podResource: "PodList"}, pod)
	native := &Native{client: client}
	obs := biz.Observation{VM: v.Native, VMI: &biz.ObjectRef{Kind: "VirtualMachineInstance", Namespace: vmLogTenant, Name: v.Native.Name, UID: vmLogVMIUID}, Launcher: &biz.ObjectRef{Kind: "Pod", Namespace: vmLogTenant, Name: vmLogPod, UID: vmLogPodUID}, Running: true}
	return native, v, obs, client, pod
}

func TestVMLogsPreferGuestConsoleAndReachEOF(t *testing.T) {
	native, v, obs, _, _ := vmLogFixture(t)
	source := &vmLogSource{body: &vmLogReader{Reader: strings.NewReader("guest boot\n")}}
	native.logSource, native.logLimits = source, biz.LogLimits{}
	var frames []biz.LogFrame
	if e := native.StreamLogs(context.Background(), v, obs, 100, false, func(f biz.LogFrame) error {
		frames = append(frames, f)
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if source.container != guestConsoleContainer {
		t.Fatalf("container: %s", source.container)
	}
	if len(frames) != 3 || frames[0].Kind != "READY" || frames[1].Kind != "DATA" || string(frames[1].Data) != "guest boot\n" || frames[2].Reason != "EOF" {
		t.Fatalf("frames: %+v", frames)
	}
}

func TestVMLogsVerifyBeforeReady(t *testing.T) {
	native, v, obs, client, pod := vmLogFixture(t)
	body := &vmLogReader{Reader: strings.NewReader("private")}
	source := &vmLogSource{body: body, open: func() error {
		changed := pod.DeepCopy()
		changed.SetUID("replacement-pod")
		_, e := client.Resource(podResource).Namespace(vmLogTenant).Update(context.Background(), changed, metav1.UpdateOptions{})
		return e
	}}
	native.logSource, native.logLimits = source, biz.LogLimits{}
	frames := 0
	e := native.StreamLogs(context.Background(), v, obs, 100, false, func(biz.LogFrame) error { frames++; return nil })
	if !errors.Is(e, biz.ErrLogTarget) || frames != 0 || !body.closed {
		t.Fatalf("replacement pod leaked data: %v frames=%d closed=%v", e, frames, body.closed)
	}
}

func TestVMLogsPodChangeEndsStream(t *testing.T) {
	native, v, obs, client, pod := vmLogFixture(t)
	reader, writer := io.Pipe()
	defer writer.Close()
	native.logSource = &vmLogSource{body: reader}
	native.logLimits = biz.LogLimits{TargetPollSeconds: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{})
	var mu sync.Mutex
	var frames []biz.LogFrame
	done := make(chan error, 1)
	go func() {
		done <- native.StreamLogs(ctx, v, obs, 100, true, func(f biz.LogFrame) error {
			mu.Lock()
			defer mu.Unlock()
			frames = append(frames, f)
			if f.Kind == "READY" {
				close(ready)
			}
			return nil
		})
	}()
	<-ready
	if _, e := writer.Write([]byte("live")); e != nil {
		t.Fatal(e)
	}
	changed := pod.DeepCopy()
	changed.SetUID("new-pod")
	if _, e := client.Resource(podResource).Namespace(vmLogTenant).Update(context.Background(), changed, metav1.UpdateOptions{}); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(frames) < 2 || frames[len(frames)-1].Kind != "END" || frames[len(frames)-1].Reason != "POD_CHANGED" {
		t.Fatalf("lifecycle: %+v", frames)
	}
	if _, e := writer.Write([]byte("after close")); e == nil {
		t.Fatal("upstream body remained open")
	}
}

func TestVMLogsRequireLauncher(t *testing.T) {
	native, v, _, _, _ := vmLogFixture(t)
	native.logSource = &vmLogSource{body: &vmLogReader{Reader: strings.NewReader("x")}}
	if e := native.StreamLogs(context.Background(), v, biz.Observation{}, 100, false, func(biz.LogFrame) error { return nil }); !errors.Is(e, biz.ErrLogTarget) {
		t.Fatalf("missing launcher accepted: %v", e)
	}
}
