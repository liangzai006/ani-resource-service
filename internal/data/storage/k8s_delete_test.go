package storage

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestPVCDeleteAlwaysUsesFrozenUIDPrecondition(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	calls := 0
	client.PrependReactor("delete", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		options := action.(ktesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions == nil || options.Preconditions.UID == nil || string(*options.Preconditions.UID) != "frozen-original-uid" || action.GetNamespace() != "tenant-a" {
			t.Fatal("PVC delete lost original UID or Namespace", action)
		}
		return true, nil, context.DeadlineExceeded
	})
	adapter, _ := NewKubernetes(client)
	if err := adapter.DeleteVolume(context.Background(), "tenant-a", "dataset", "frozen-original-uid"); !errors.Is(err, biz.ErrBackendUnavailable) || calls != 1 {
		t.Fatal("unknown Kubernetes response was not preserved", calls, err)
	}
	if err := adapter.DeleteVolume(context.Background(), "tenant-a", "dataset", ""); !errors.Is(err, biz.ErrInvalid) || calls != 1 {
		t.Fatal("PVC delete with empty UID reached Kubernetes", calls, err)
	}
}
