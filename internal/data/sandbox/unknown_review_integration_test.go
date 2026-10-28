//go:build sandboxintegration

package sandbox_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	kt "k8s.io/client-go/testing"
)

func TestUnknownCreateReviewIsReadOnlyAndDoesNotInferAbsence(t *testing.T) {
	f := sandboxDatabase(t)
	ctx, c := fixtureCreate()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	m := biz.Mutation{ID: uuid.NewString(), Kind: biz.MutationCreatePod, OperationID: c.OperationID, Version: 1}
	raw, _ := json.Marshal(m)
	if _, err := f.runtime.Exec(ctx, `UPDATE sandbox.sandboxes SET pending_mutation=$3,observed='unknown' WHERE tenant_id=$1 AND sandbox_id=$2`, c.TenantID, c.ID, raw); err != nil {
		t.Fatal(err)
	}
	before, err := f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var workBefore, workAfter []byte
	if err = f.runtime.QueryRow(ctx, `SELECT to_jsonb(w) FROM sandbox.work w WHERE tenant_id=$1 AND sandbox_id=$2`, c.TenantID, c.ID).Scan(&workBefore); err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("create", "selfsubjectreviews", func(kt.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"userInfo": map[string]any{"username": "operator/alice"}}}}, nil
	})
	provider, err := data.NewKubernetesProvider(client, c.Spec.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := f.repo.ReviewUnknownCreate(ctx, provider, c.TenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pending == nil || *report.Pending != m || report.Pod.Status != "not_found" || report.PVC.Status != "not_found" || report.PV.Status != "identity_not_recorded" || report.KubernetesUser != "operator/alice" || report.DatabaseUser == "" || report.AuditStatus != "not_recorded" || report.ClusterIdentityStatus != "physical_cluster_not_authenticated" || report.Interpretation == "" {
		t.Fatalf("incorrect diagnostic: %+v", report)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" && !(a.GetVerb() == "create" && a.GetResource().Resource == "selfsubjectreviews") {
			t.Fatalf("unexpected Kubernetes mutation: %+v", a)
		}
	}
	after, err := f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("review modified sandbox: %+v %v", after, err)
	}
	if err = f.runtime.QueryRow(ctx, `SELECT to_jsonb(w) FROM sandbox.work w WHERE tenant_id=$1 AND sandbox_id=$2`, c.TenantID, c.ID).Scan(&workAfter); err != nil {
		t.Fatal(err)
	}
	if string(workBefore) != string(workAfter) {
		t.Fatal("review modified work lease")
	}
	mismatch, err := data.NewKubernetesProvider(client, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	count := len(client.Actions())
	r, err := f.repo.ReviewUnknownCreate(ctx, mismatch, c.TenantID, c.ID)
	if err != nil || r.Pod.Status != "not_observed_cluster_mismatch" || len(client.Actions()) != count {
		t.Fatalf("wrong logical cluster contacted: %+v %v", r, err)
	}
	client.PrependReactor("get", "pods", func(kt.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "pod", errors.New("denied"))
	})
	r, err = f.repo.ReviewUnknownCreate(ctx, provider, c.TenantID, c.ID)
	if err != nil || r.Pod.Status != "forbidden" || r.Pod.UID != "" {
		t.Fatalf("forbidden confused with absence: %+v %v", r, err)
	}
	if _, err = f.repo.ReviewUnknownCreate(ctx, provider, uuid.NewString(), c.ID); !errors.Is(err, biz.ErrNotFound) {
		t.Fatalf("cross-tenant review: %v", err)
	}
}
