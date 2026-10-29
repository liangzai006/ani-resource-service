package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func runtimeClassObject(name, handler string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass",
		"metadata": map[string]any{"name": name}, "handler": handler,
	}}
}

func TestRuntimeClassSelection(t *testing.T) {
	deleting := runtimeClassObject("sandbox-kata", "kata-qemu")
	timestamp := metav1.NewTime(time.Unix(1, 0))
	deleting.SetDeletionTimestamp(&timestamp)
	for _, tc := range []struct {
		name    string
		objects []runtime.Object
		want    string
	}{
		{"preferred class", []runtime.Object{runtimeClassObject("sandbox-kata", "kata-qemu"), runtimeClassObject("kata", "kata-qemu-runtime-rs")}, "sandbox-kata"},
		{"cluster kata fallback", []runtime.Object{runtimeClassObject("kata", "kata-qemu-runtime-rs")}, "kata"},
		{"exact kata handler", []runtime.Object{runtimeClassObject("kata", "kata")}, "kata"},
		{"missing", nil, ""},
		{"non kata preferred fails closed", []runtime.Object{runtimeClassObject("sandbox-kata", "runc"), runtimeClassObject("kata", "kata-qemu")}, ""},
		{"misleading prefix fails closed", []runtime.Object{runtimeClassObject("kata", "katapult")}, ""},
		{"deleting preferred fails closed", []runtime.Object{deleting, runtimeClassObject("kata", "kata-qemu")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), tc.objects...)
			got, err := (RuntimeClass{Client: client}).ResolveSandboxRuntimeClass(context.Background())
			if tc.want == "" {
				if got != "" || !errors.Is(err, biz.ErrDependencyUnavailable) {
					t.Fatalf("got=%q err=%v", got, err)
				}
			} else if got != tc.want || err != nil {
				t.Fatalf("got=%q err=%v", got, err)
			}
		})
	}
}

func TestRuntimeClassAccessFailureDoesNotFallback(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), runtimeClassObject("kata", "kata-qemu"))
	client.PrependReactor("get", "runtimeclasses", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "node.k8s.io", Resource: "runtimeclasses"}, action.(ktesting.GetAction).GetName(), errors.New("forbidden"))
	})
	if _, err := (RuntimeClass{Client: client}).ResolveSandboxRuntimeClass(context.Background()); !errors.Is(err, biz.ErrDependencyUnavailable) {
		t.Fatalf("got %v", err)
	}
	if len(client.Actions()) != 1 {
		t.Fatalf("unexpected fallback: %v", client.Actions())
	}
	if _, err := (RuntimeClass{}).ResolveSandboxRuntimeClass(context.Background()); !errors.Is(err, biz.ErrDependencyUnavailable) {
		t.Fatalf("nil client: %v", err)
	}
}

func TestFrozenRuntimeClassValidatedIndependentlyOfPreference(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), runtimeClassObject("kata", "kata-qemu"))
	r := RuntimeClass{Client: client}
	selected, err := r.ResolveSandboxRuntimeClass(context.Background())
	if err != nil || selected != "kata" {
		t.Fatalf("initial=%q err=%v", selected, err)
	}
	if _, err := client.Resource(runtimeClassGVR).Create(context.Background(), runtimeClassObject("sandbox-kata", "kata-qemu"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	selected, err = r.ResolveSandboxRuntimeClass(context.Background())
	if err != nil || selected != "sandbox-kata" {
		t.Fatalf("new preference=%q err=%v", selected, err)
	}
	if err := r.ValidateSandboxRuntimeClass(context.Background(), "kata"); err != nil {
		t.Fatalf("existing frozen class rejected: %v", err)
	}
	if err := r.ValidateSandboxRuntimeClass(context.Background(), "nvidia"); !errors.Is(err, biz.ErrInvalid) {
		t.Fatalf("unsupported class accepted: %v", err)
	}
	if err := client.Resource(runtimeClassGVR).Delete(context.Background(), "kata", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateSandboxRuntimeClass(context.Background(), "kata"); !errors.Is(err, biz.ErrDependencyUnavailable) {
		t.Fatalf("missing frozen class accepted: %v", err)
	}
}
