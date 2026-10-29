package sandbox

import (
	"context"
	"fmt"
	"strings"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var runtimeClassGVR = schema.GroupVersionResource{Group: "node.k8s.io", Version: "v1", Resource: "runtimeclasses"}

// RuntimeClass reads only the two established Kata class names, in preference
// order. NotFound is the only condition that permits trying the next name.
type RuntimeClass struct{ Client dynamic.Interface }

func (r RuntimeClass) ResolveSandboxRuntimeClass(ctx context.Context) (string, error) {
	if r.Client == nil {
		return "", biz.ErrDependencyUnavailable
	}
	for _, name := range []string{"sandbox-kata", "kata"} {
		err := r.validate(ctx, name)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("%w: no supported Kata runtime class", biz.ErrDependencyUnavailable)
}

// ValidateSandboxRuntimeClass checks the frozen name independently of current
// preference, so adding a preferred class cannot strand an accepted intent.
func (r RuntimeClass) ValidateSandboxRuntimeClass(ctx context.Context, name string) error {
	if name != "sandbox-kata" && name != "kata" {
		return biz.ErrInvalid
	}
	if r.Client == nil {
		return biz.ErrDependencyUnavailable
	}
	return r.validate(ctx, name)
}

func (r RuntimeClass) validate(ctx context.Context, name string) error {
	class, err := r.Client.Resource(runtimeClassGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("%w: read runtime class %s: %w", biz.ErrDependencyUnavailable, name, err)
	}
	handler, found, err := unstructured.NestedString(class.Object, "handler")
	if err != nil || !found || (handler != "kata" && !strings.HasPrefix(handler, "kata-")) || class.GetDeletionTimestamp() != nil {
		return fmt.Errorf("%w: runtime class %s is not a usable Kata class", biz.ErrDependencyUnavailable, name)
	}
	return nil
}

var _ biz.RuntimeClassResolver = RuntimeClass{}
