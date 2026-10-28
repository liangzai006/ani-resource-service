package data_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
)

// PostgreSQL, admission, replay, authorization and worker logic are production
// code. Only the external Kubernetes HTTP boundary is controlled; these tests
// are not evidence of installed KCN or public data-plane acceptance.
type defaultEgressFixture struct {
	*egressFixture
	api      *controlled.Server
	repo     *data.DefaultVPCEgressPostgres
	usecase  *biz.DefaultVPCEgressUseCase
	provider *data.DefaultEgressKCProvider
	worker   *biz.DefaultVPCEgressWorker
	identity biz.DefaultVPCIdentity
	policy   biz.WorkerPolicy
}

func TestCurrentDefaultVPCEgress(t *testing.T) {
	f := newDefaultEgressFixture(t, nil)
	if _, err := f.usecase.GetCurrent(f.ctx); err == nil {
		t.Fatal("empty current outlet must be not found")
	}
	created := f.create(t, "current-default-egress")
	current, err := f.usecase.GetCurrent(f.ctx)
	if err != nil || current.ID != created.ID || current.Identity != f.identity {
		t.Fatalf("current outlet not discoverable: %v %v", current, err)
	}
	rows, total, err := f.e.ListPlatformEIPs(f.ctx, biz.PlatformEIPFilter{PageWindow: biz.PageWindow{Limit: 20}})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].ID != current.EIPID || rows[0].ManagedBy != "system" || rows[0].TenantID != "" || rows[0].BindingTarget.ID != current.ID {
		t.Fatalf("system EIP missing from platform inventory: %+v total=%d err=%v", rows, total, err)
	}
	if _, err = f.e.GetPlatformEIP(f.ctx, current.EIPID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.ReleasePlatformEIP(f.ctx, current.EIPID); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatalf("system EIP independently released: %v", err)
	}
	rows, total, err = f.e.ListPlatformEIPs(f.ctx, biz.PlatformEIPFilter{PageWindow: biz.PageWindow{Limit: 20}, TenantID: f.tenant})
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("system EIP leaked into tenant filter: %+v %d %v", rows, total, err)
	}
	tenant := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: "11111111-1111-4111-8111-111111111111"})
	if _, err = f.usecase.GetCurrent(tenant); err == nil {
		t.Fatal("tenant discovered platform outlet")
	}
	if _, err = f.usecase.Delete(f.ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	f.driveDefault(t, created.ID, func(v biz.DefaultVPCEgress) bool { return v.State == biz.Deleted })
	if _, err = f.usecase.GetCurrent(f.ctx); err == nil {
		t.Fatal("retired outlet returned as current")
	}
}

func newDefaultEgressFixture(t *testing.T, intercept func(http.ResponseWriter, *http.Request, *controlled.Server) bool) *defaultEgressFixture {
	t.Helper()
	f, api, _, kubeconfig := newEgressKCFixture(t, intercept)
	vpcUID := seedIntranetInfrastructure(api)
	clusterUID := uuid.NewString()
	api.Change("namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": clusterUID}}, false)
	// Tenant allocation remains closed throughout this platform-only lifecycle.
	f.setPool(t, biz.PlatformIntent{Kind: "set_pool_allocation", ID: f.pool.ID, IdempotencyKey: "close-for-platform", Enabled: false})
	f.ctx = biz.WithEgressCaller(context.Background(), biz.EgressCaller{PlatformAdministrator: true, Attribution: biz.Attribution{Actor: "platform-default-egress-test", DirectCaller: "fixture"}})
	kc, err := data.OpenKCProvider(f.p, kubeconfig, data.KCClientPolicy{QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	identity := biz.DefaultVPCIdentity{ClusterID: "test-cluster", ClusterUID: clusterUID, Namespace: "kcn-system", Name: "kcn-cluster", UID: vpcUID}
	provider, err := data.NewDefaultEgressKCProvider(kc, identity)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := data.NewDefaultVPCEgressRepository(f.p, identity, provider, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := biz.NewDefaultVPCEgress(repo, biz.ContextEgressAuthorization{})
	if err != nil {
		t.Fatal(err)
	}
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 15 * time.Millisecond
	policy.RetryMin = 5 * time.Millisecond
	policy.RetryMax = 20 * time.Millisecond
	d := &defaultEgressFixture{egressFixture: f, api: api, repo: repo, usecase: u, provider: provider, identity: identity, policy: policy}
	d.restart(t)
	return d
}

func (f *defaultEgressFixture) restart(t *testing.T) {
	t.Helper()
	w, err := biz.NewDefaultVPCEgressWorker(f.repo, f.provider, uuid.NewString(), f.policy)
	if err != nil {
		t.Fatal(err)
	}
	f.worker = w
}
func (f *defaultEgressFixture) create(t *testing.T, key string) biz.DefaultVPCEgress {
	t.Helper()
	v, err := f.usecase.Create(f.ctx, biz.DefaultVPCEgressIntent{Name: "platform-exit", PoolID: f.pool.ID, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func (f *defaultEgressFixture) driveDefault(t *testing.T, id string, done func(biz.DefaultVPCEgress) bool) biz.DefaultVPCEgress {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var v biz.DefaultVPCEgress
	for time.Now().Before(deadline) {
		if _, err := f.worker.Step(f.ctx); err != nil {
			t.Fatal(err)
		}
		var err error
		v, err = f.usecase.Get(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if done(v) {
			return v
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("platform default egress did not converge: %+v", v)
	return v
}
func (f *defaultEgressFixture) state(t *testing.T, id string, state biz.ResourceState) biz.DefaultVPCEgress {
	t.Helper()
	return f.driveDefault(t, id, func(v biz.DefaultVPCEgress) bool { return v.State == state })
}

func TestDefaultVPCEgressKCRealBusinessLifecycle(t *testing.T) {
	var holdUnbinding atomic.Bool
	f := newDefaultEgressFixture(t, func(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
		if !holdUnbinding.Load() || r.Method != http.MethodDelete || !strings.Contains(r.URL.Path, "/snats/") {
			return false
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		snat := api.Object("snats", "kcn-system", name)
		if snat == nil {
			return false
		}
		eipName, _ := snat["spec"].(map[string]any)["eip"].(string)
		before := api.Object("eips", "kcn-system", eipName)
		api.ServeHTTP(w, r)
		if before != nil {
			after := api.Object("eips", "kcn-system", eipName)
			after["status"] = before["status"]
			api.Change("eips", after, false)
		}
		return true
	})
	nativeBefore, _ := json.Marshal(f.api.Object("vpcs", "kcn-system", "kcn-cluster"))
	i := biz.DefaultVPCEgressIntent{Name: "platform-exit", PoolID: f.pool.ID, IdempotencyKey: "platform-create"}
	if _, err := f.usecase.Create(context.Background(), i); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("unauthorized platform admission", err)
	}
	tenant := biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: uuid.NewString(), Attribution: biz.Attribution{Actor: "tenant", DirectCaller: "test"}})
	if _, err := f.usecase.Create(tenant, i); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatal("tenant admitted to platform aggregate", err)
	}
	accepted := f.create(t, i.IdempotencyKey)
	replay, err := f.usecase.Create(f.ctx, i)
	if err != nil || replay.ID != accepted.ID || replay.Version != accepted.Version || replay.LastOperationID != accepted.LastOperationID {
		t.Fatal("acceptance was not replayed exactly", replay, err)
	}
	i.Name = "different"
	if _, err = f.usecase.Create(f.ctx, i); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatal("key accepted another fingerprint", err)
	}
	i.Name = "platform-exit"
	i.IdempotencyKey = "second-active"
	if _, err = f.usecase.Create(f.ctx, i); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("second active default egress admitted", err)
	}
	if _, err = f.e.DeletePlatform(f.ctx, "public_pool", f.pool.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("pool delete ignored platform reservation", err)
	}
	available := f.state(t, accepted.ID, biz.Available)
	if available.PendingUnknown || available.PendingAction != "" || available.ObservationStale || available.Address == "" || available.EIPProviderUID == "" || available.SnatProviderUID == "" || available.AppliedEnabled == nil || !*available.AppliedEnabled || available.TargetGeneration < 1 || available.Identity != f.identity {
		t.Fatal("incomplete actual binding proof", available)
	}
	var tenantRows int
	if err = f.owner.QueryRow(f.ctx, "SELECT (SELECT count(*) FROM network_eips)+(SELECT count(*) FROM network_snat_bindings)").Scan(&tenantRows); err != nil || tenantRows != 0 {
		t.Fatal("platform aggregate borrowed tenant rows", tenantRows, err)
	}
	eip := f.api.Object("eips", f.identity.Namespace, available.EIPProviderName)
	bound := eip["status"].(map[string]any)["boundResource"].(map[string]any)
	saved := bound["observedGeneration"]
	bound["observedGeneration"] = 0
	f.api.Change("eips", eip, false)
	degraded := f.state(t, accepted.ID, biz.Degraded)
	if degraded.AppliedEnabled != nil || degraded.EIPProviderUID != available.EIPProviderUID || degraded.SnatProviderUID != available.SnatProviderUID {
		t.Fatal("generation drift accepted or identities replaced", degraded)
	}
	bound["observedGeneration"] = saved
	f.api.Change("eips", eip, false)
	f.state(t, accepted.ID, biz.Available)
	holdUnbinding.Store(true)
	if _, err = f.usecase.Delete(f.ctx, accepted.ID); err != nil {
		t.Fatal(err)
	}
	waiting := f.driveDefault(t, accepted.ID, func(v biz.DefaultVPCEgress) bool { return v.State == biz.Deleting && v.Reason == biz.ResourceInUse })
	if waiting.PendingAction != "delete_snat" {
		t.Fatal("pending delete marker lost", waiting)
	}
	f.api.Backend.Mu.Lock()
	eipDeletes := f.api.Backend.Deletes["eips"]
	f.api.Backend.Mu.Unlock()
	if eipDeletes != 0 {
		t.Fatal("EIP deleted before actual unbinding")
	}
	if _, err = f.e.DeletePlatform(f.ctx, "public_pool", f.pool.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("blocked deletion released pool reservation", err)
	}
	eip = f.api.Object("eips", f.identity.Namespace, available.EIPProviderName)
	eip["status"].(map[string]any)["boundResource"] = nil
	eip["status"].(map[string]any)["phase"] = "Available"
	f.api.Change("eips", eip, false)
	f.restart(t)
	deleted := f.state(t, accepted.ID, biz.Deleted)
	if deleted.PendingUnknown || deleted.PendingAction != "" || deleted.OperationState != "succeeded" || f.api.Object("snats", f.identity.Namespace, available.SnatProviderName) != nil || f.api.Object("eips", f.identity.Namespace, available.EIPProviderName) != nil {
		t.Fatal("tombstone before final disappearance", deleted)
	}
	var active int
	if err = f.owner.QueryRow(f.ctx, "SELECT count(*) FROM network_default_vpc_egresses WHERE pool_id=$1 AND retired_at IS NULL", f.pool.ID).Scan(&active); err != nil || active != 0 {
		t.Fatal("pool reservation not released", active, err)
	}
	nativeAfter, _ := json.Marshal(f.api.Object("vpcs", "kcn-system", "kcn-cluster"))
	if string(nativeBefore) != string(nativeAfter) {
		t.Fatal("default VPC was modified")
	}
	if f.api.Backend.Invalid != "" {
		t.Fatal(f.api.Backend.Invalid)
	}
}

func TestDefaultVPCEgressKCLostCreateResponseRecoversSameIdentity(t *testing.T) {
	var lose atomic.Bool
	lose.Store(true)
	f := newDefaultEgressFixture(t, func(w http.ResponseWriter, r *http.Request, api *controlled.Server) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/eips") || !lose.CompareAndSwap(true, false) {
			return false
		}
		recorded := httptest.NewRecorder()
		api.ServeHTTP(recorded, r)
		if recorded.Code != http.StatusCreated {
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			return true
		}
		http.Error(w, "lost response after provider persisted the object", http.StatusGatewayTimeout)
		return true
	})
	accepted := f.create(t, "lost-create-response")
	pending := f.driveDefault(t, accepted.ID, func(v biz.DefaultVPCEgress) bool { return v.PendingAction == "create_eip" && v.PendingUnknown })
	if pending.EIPProviderUID != "" {
		t.Fatal("provider UID invented before observation", pending)
	}
	f.restart(t)
	v := f.state(t, accepted.ID, biz.Available)
	f.api.Backend.Mu.Lock()
	creates := f.api.Backend.Creates["eips"]
	f.api.Backend.Mu.Unlock()
	if creates != 1 || v.ID != accepted.ID || v.EIPProviderName != accepted.EIPProviderName || v.EIPProviderUID == "" {
		t.Fatal("restart changed reservation or repeated create", creates, v)
	}
	if _, err := f.usecase.Delete(f.ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	f.state(t, v.ID, biz.Deleted)
}

func TestDefaultVPCEgressKCUnknownUndeliveredCreateRetainsReservation(t *testing.T) {
	f := newDefaultEgressFixture(t, func(w http.ResponseWriter, r *http.Request, _ *controlled.Server) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/eips") {
			return false
		}
		http.Error(w, "provider result unknown", http.StatusGatewayTimeout)
		return true
	})
	accepted := f.create(t, "unknown-undelivered")
	f.driveDefault(t, accepted.ID, func(v biz.DefaultVPCEgress) bool { return v.PendingUnknown && v.PendingAction == "create_eip" })
	f.restart(t)
	if _, err := f.usecase.Delete(f.ctx, accepted.ID); err != nil {
		t.Fatal(err)
	}
	v := f.driveDefault(t, accepted.ID, func(v biz.DefaultVPCEgress) bool { return v.State == biz.Deleting && v.Reason == biz.ProviderUnknown })
	if !v.PendingUnknown || v.PendingAction != "create_eip" || v.EIPProviderName != accepted.EIPProviderName {
		t.Fatal("unknown result lost durable identity", v)
	}
	if _, err := f.e.DeletePlatform(f.ctx, "public_pool", f.pool.ID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("unknown result released reference", err)
	}
	var active int
	if err := f.owner.QueryRow(f.ctx, "SELECT count(*) FROM network_default_vpc_egresses WHERE retired_at IS NULL").Scan(&active); err != nil || active != 1 {
		t.Fatal("unknown result retired reservation", active, err)
	}
}

func TestDefaultVPCEgressKCExactIdentityAndStaleLease(t *testing.T) {
	f := newDefaultEgressFixture(t, nil)
	vpc := f.api.Object("vpcs", f.identity.Namespace, f.identity.Name)
	vpc["metadata"].(map[string]any)["uid"] = uuid.NewString()
	f.api.Change("vpcs", vpc, false)
	if _, err := f.usecase.Create(f.ctx, biz.DefaultVPCEgressIntent{Name: "wrong-vpc", PoolID: f.pool.ID, IdempotencyKey: "wrong-vpc"}); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatal("accepted another default VPC UID", err)
	}
	vpc["metadata"].(map[string]any)["uid"] = f.identity.UID
	f.api.Change("vpcs", vpc, false)
	v := f.create(t, "lease")
	x, found, err := f.repo.ClaimDefaultVPCEgress(f.ctx, uuid.NewString(), 20*time.Second)
	if err != nil || !found {
		t.Fatal("claim", found, err)
	}
	if _, err = f.usecase.Delete(f.ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.BeginDefaultVPCEgressMutation(f.ctx, x, "create_eip"); !errors.Is(err, biz.ErrLeaseLost) {
		t.Fatal("superseded create retained mutation lease", err)
	}
	f.state(t, v.ID, biz.Deleted)
}
