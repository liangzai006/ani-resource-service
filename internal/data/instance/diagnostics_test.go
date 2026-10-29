package instance

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func diagnosticFixture(t *testing.T, gpu bool) (*Notebook, biz.Instance, *fake.FakeDynamicClient, *unstructured.Unstructured) {
	t.Helper()
	const id = "11111111-1111-4111-8111-111111111111"
	i := biz.Instance{TenantID: id, ID: id, CreateOperationID: id, CreateRequestHash: strings.Repeat("a", 64), BusinessDigest: strings.Repeat("b", 64), Version: 1, Desired: biz.Running, RuntimeDesired: biz.Running, MountsInitialized: true, CurrentMounts: []biz.VolumeMount{}, Spec: biz.Spec{ContractVersion: 2, WorkspaceMountPath: "/workspace", InstanceKind: biz.KindContainer, ComputeType: biz.TypeCPU, Scheduling: biz.Scheduling{PodPlacement: "NONE"}, ImageReference: "registry.invalid/work@sha256:" + strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("c", 64), CPU: "1", Memory: "2Gi", UID: 1000, GID: 1000, Shell: "/bin/sh", ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", ClusterID: id, Namespace: "tenant-" + id, NotebookName: "instance-" + id, PVCName: "workspace-" + id}}
	if gpu {
		i.Spec.ComputeType = biz.TypeGPU
		i.Spec.GPU = biz.GPU{Scheduler: "volcano", RuntimeClass: "nvidia", Queue: "default", Digest: strings.Repeat("d", 64), Limits: map[string]string{"volcano.sh/vgpu-number": "1", "volcano.sh/vgpu-cores": "100", "volcano.sh/vgpu-memory": "15360"}, NodeLabels: map[string]string{"gpu": "test"}}
	}
	notebook, e := RenderNotebook(i)
	if e != nil {
		t.Fatal(e)
	}
	notebook.SetUID("notebook-uid")
	i.NotebookUID = "notebook-uid"
	template, _, _ := unstructured.NestedMap(notebook.Object, "spec", "template")
	pod := &unstructured.Unstructured{Object: template}
	pod.SetAPIVersion("v1")
	pod.SetKind("Pod")
	pod.SetNamespace(i.Spec.Namespace)
	pod.SetName(i.Spec.NotebookName + "-0")
	pod.SetUID("pod-uid")
	pod.SetLabels(notebook.GetLabels())
	pod.SetAnnotations(notebook.GetAnnotations())
	controller := true
	set := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": i.Spec.NotebookName, "namespace": i.Spec.Namespace, "uid": "set-uid"}, "spec": map[string]any{"replicas": int64(1)}}}
	set.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubeflow.org/v1", Kind: "Notebook", Name: i.Spec.NotebookName, UID: notebook.GetUID(), Controller: &controller}})
	pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: i.Spec.NotebookName, UID: set.GetUID(), Controller: &controller}})
	pod.Object["status"] = map[string]any{"phase": "Running", "podIP": "10.1.2.3", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": biz.Container, "ready": true}}}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{podGVR: "PodList", eventGVR: "EventList"}, notebook, set, pod)
	n, e := NewNotebook(client)
	if e != nil {
		t.Fatal(e)
	}
	if e = n.ConfigureSigningKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))); e != nil {
		t.Fatal(e)
	}
	return n, i, client, pod
}
func TestDiagnosticReadOnlySafeProjection(t *testing.T) {
	n, i, client, pod := diagnosticFixture(t, true)
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	c := containers[0].(map[string]any)
	env, _, _ := unstructured.NestedSlice(c, "env")
	env = append(env, map[string]any{"name": "TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "hidden-secret-name", "key": "hidden-secret-key"}}}, map[string]any{"name": "PUBLIC", "value": "hidden-literal-value"})
	c["env"] = env
	_ = unstructured.SetNestedSlice(pod.Object, containers, "spec", "containers")
	if _, e := client.Resource(podGVR).Namespace(i.Spec.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); e != nil {
		t.Fatal(e)
	}
	client.ClearActions()
	v, e := n.ReadRuntime(context.Background(), i)
	if e != nil || v.Pod == nil || !v.Pod.Ready || v.StatefulSet == nil || v.StatefulSet.ReadyReplicas != nil {
		t.Fatal("runtime null/readiness", v, e)
	}
	raw, _ := json.Marshal(v)
	for _, secret := range []string{"hidden-secret-name", "hidden-secret-key", "hidden-literal-value"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("runtime leaked source contents")
		}
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" {
			t.Fatal("diagnostic wrote Kubernetes", a.GetVerb())
		}
	}
	old := i
	i.Spec.Namespace = "foreign"
	v, e = n.ReadRuntime(context.Background(), i)
	if e != nil || v.Pod != nil {
		t.Fatal("foreign namespace unexpectedly resolved", e)
	}
	i = old
	refs := pod.GetOwnerReferences()
	refs[0].UID = "foreign-set"
	pod.SetOwnerReferences(refs)
	_, _ = client.Resource(podGVR).Namespace(i.Spec.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
	if _, e = n.ReadRuntime(context.Background(), i); !errors.Is(e, biz.ErrOwnership) {
		t.Fatal("foreign owner exposed", e)
	}
}
func TestDiagnosticEventsUIDBoundsAndUnknowns(t *testing.T) {
	n, i, client, _ := diagnosticFixture(t, false)
	calls := 0
	client.PrependReactor("list", "events", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		a := action.(ktesting.ListAction)
		if !strings.Contains(a.GetListRestrictions().Fields.String(), "involvedObject.uid=pod-uid") {
			t.Fatal("event UID selector missing")
		}
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion("v1")
		list.SetKind("EventList")
		for k := 0; k < 200; k++ {
			idx := (calls-1)*200 + k
			event := unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Event", "metadata": map[string]any{"namespace": i.Spec.Namespace, "uid": fmt.Sprintf("event-%04d", idx)}, "involvedObject": map[string]any{"kind": "Pod", "uid": "pod-uid"}, "reason": "Scheduled"}}
			if idx == 0 {
				event.Object["involvedObject"].(map[string]any)["uid"] = "old-pod-uid"
			}
			if idx == 1 {
				event.Object["series"] = map[string]any{"lastObservedTime": "2026-10-16T10:00:00Z", "count": int64(2)}
			}
			list.Items = append(list.Items, event)
		}
		list.SetContinue(fmt.Sprintf("page-%d", calls))
		return true, list, nil
	})
	out, e := n.ReadEvents(context.Background(), i)
	if e != nil || calls != 5 || !out.Truncated || len(out.Items) != 999 || out.PodUID == nil || *out.PodUID != "pod-uid" {
		t.Fatal("bounded current UID events", len(out.Items), calls, e)
	}
	if out.Items[0].LastOccurredAt == nil || out.Items[0].Count == nil || *out.Items[0].Count != 2 || out.Items[1].Count != nil || out.Items[1].LastOccurredAt != nil {
		t.Fatal("unknown event time/count fabricated")
	}
}

type testLogSource struct {
	open func(context.Context, string, string, uint32, bool) (io.ReadCloser, error)
}

func (s testLogSource) Open(ctx context.Context, ns, pod string, tail uint32, follow bool) (io.ReadCloser, error) {
	return s.open(ctx, ns, pod, tail, follow)
}

type closeReader struct {
	io.Reader
	closed bool
}

func (r *closeReader) Close() error { r.closed = true; return nil }
func TestLogsVerifyBeforeReadyAndRead(t *testing.T) {
	n, i, client, pod := diagnosticFixture(t, false)
	body := &closeReader{Reader: strings.NewReader("private logs")}
	n.logSource = testLogSource{open: func(context.Context, string, string, uint32, bool) (io.ReadCloser, error) {
		changed := pod.DeepCopy()
		changed.SetUID("replacement-pod")
		_, e := client.Resource(podGVR).Namespace(i.Spec.Namespace).Update(context.Background(), changed, metav1.UpdateOptions{})
		return body, e
	}}
	frames := 0
	e := n.StreamLogs(context.Background(), i, 100, false, func(biz.LogFrame) error { frames++; return nil })
	if !errors.Is(e, biz.ErrPodChanged) || frames != 0 || !body.closed {
		t.Fatal("UID replacement released data or leaked body", frames, body.closed, e)
	}
}
func TestLogsFramesCancellationAndPodChange(t *testing.T) {
	n, i, client, pod := diagnosticFixture(t, false)
	reader, writer := io.Pipe()
	defer writer.Close()
	n.logSource = testLogSource{open: func(context.Context, string, string, uint32, bool) (io.ReadCloser, error) { return reader, nil }}
	n.logLimits = biz.LogLimits{TargetPollSeconds: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{})
	var mu sync.Mutex
	frames := []biz.LogFrame{}
	done := make(chan error, 1)
	go func() {
		done <- n.StreamLogs(ctx, i, 100, true, func(f biz.LogFrame) error {
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
	if _, e := writer.Write([]byte("汉字\n")); e != nil {
		t.Fatal(e)
	}
	changed := pod.DeepCopy()
	changed.SetUID("new-pod")
	if _, e := client.Resource(podGVR).Namespace(i.Spec.Namespace).Update(context.Background(), changed, metav1.UpdateOptions{}); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(frames) < 2 || frames[0].Kind != "READY" || frames[len(frames)-1].Reason != "POD_CHANGED" {
		t.Fatal("log lifecycle", frames)
	}
	if _, e := writer.Write([]byte("must fail after close")); e == nil {
		t.Fatal("upstream body remained open")
	}
}
func TestLogsEOFAndClientCancellation(t *testing.T) {
	n, i, _, _ := diagnosticFixture(t, false)
	body := &closeReader{Reader: strings.NewReader(strings.Repeat("x", 40000))}
	n.logSource = testLogSource{open: func(context.Context, string, string, uint32, bool) (io.ReadCloser, error) { return body, nil }}
	frames := []biz.LogFrame{}
	if e := n.StreamLogs(context.Background(), i, 10, false, func(f biz.LogFrame) error {
		if len(f.Data) > 16*1024 {
			t.Fatal("oversize frame")
		}
		frames = append(frames, f)
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if !body.closed || frames[0].Kind != "READY" || frames[len(frames)-1].Reason != "EOF" {
		t.Fatal("EOF/body lifecycle")
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	n.logSource = testLogSource{open: func(context.Context, string, string, uint32, bool) (io.ReadCloser, error) { return reader, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	e := n.StreamLogs(ctx, i, 10, true, func(f biz.LogFrame) error {
		if f.Kind == "READY" {
			cancel()
		}
		return nil
	})
	if !errors.Is(e, context.Canceled) {
		t.Fatal("client cancellation lost", e)
	}
	if _, e = writer.Write([]byte("closed")); e == nil {
		t.Fatal("cancelled upstream remained open")
	}
}

func TestKubernetesLogClientDedicatedTimeoutAndTarget(t *testing.T) {
	n, i, _, pod := diagnosticFixture(t, false)
	upstreamClosed := make(chan struct{})
	requestSeen := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correct := r.URL.Path == "/api/v1/namespaces/"+i.Spec.Namespace+"/pods/"+pod.GetName()+"/log" && r.URL.Query().Get("container") == biz.Container && r.URL.Query().Get("tailLines") == "100" && r.URL.Query().Get("follow") == "true" && r.URL.Query().Get("previous") == ""
		requestSeen <- correct
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamClosed)
	}))
	defer server.Close()
	if e := n.ConfigureLogs(&rest.Config{Host: server.URL, Timeout: time.Nanosecond}, biz.LogLimits{}); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := false
	e := n.StreamLogs(ctx, i, 100, true, func(f biz.LogFrame) error {
		if f.Kind == "READY" {
			ready = true
			time.AfterFunc(50*time.Millisecond, cancel)
		}
		return nil
	})
	if !ready || !errors.Is(e, context.Canceled) || !<-requestSeen {
		t.Fatal("dedicated log HTTP client/target", ready, e)
	}
	select {
	case <-upstreamClosed:
	case <-time.After(time.Second):
		t.Fatal("Kubernetes log connection was not canceled")
	}
}
func TestLogsConnectionDeadline(t *testing.T) {
	n, i, _, _ := diagnosticFixture(t, false)
	reader, writer := io.Pipe()
	defer writer.Close()
	n.logSource = testLogSource{open: func(context.Context, string, string, uint32, bool) (io.ReadCloser, error) { return reader, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	var last biz.LogFrame
	if e := n.StreamLogs(ctx, i, 100, true, func(f biz.LogFrame) error { last = f; return nil }); e != nil || last.Kind != "END" || last.Reason != "TIME_LIMIT" {
		t.Fatal("connection deadline did not terminate with END", last, e)
	}
}
