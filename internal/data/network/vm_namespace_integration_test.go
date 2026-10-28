package data_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"k8s.io/client-go/rest"
)

// Only the external Kubernetes boundary is controlled. Admission, persisted
// intent/identity receipts and Network's existing durable worker are real.
type vmNetworkAPI struct {
	legacy         *testenv.KC
	mu             sync.Mutex
	objects        map[string]map[string]any
	creates        int
	denySA         bool
	loseNADReceipt bool
}

func (s *vmNetworkAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/apis/kubevirt.io/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "NotFound", "code": 404})
		return
	}
	if !strings.Contains(r.URL.Path, "/network-attachment-definitions") && !strings.Contains(r.URL.Path, "/serviceaccounts") {
		s.legacy.ServeHTTP(w, r)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	status := func(code int, reason string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "code": code, "reason": reason})
	}
	if s.objects == nil {
		s.objects = map[string]map[string]any{}
	}
	if r.Method == "GET" {
		if object := s.objects[r.URL.Path]; object != nil {
			_ = json.NewEncoder(w).Encode(object)
		} else {
			status(404, "NotFound")
		}
		return
	}
	if r.Method != "POST" {
		status(405, "MethodNotAllowed")
		return
	}
	if s.denySA && strings.HasSuffix(r.URL.Path, "/serviceaccounts") {
		status(403, "Forbidden")
		return
	}
	var object map[string]any
	if json.NewDecoder(r.Body).Decode(&object) != nil {
		status(400, "BadRequest")
		return
	}
	meta := object["metadata"].(map[string]any)
	key := r.URL.Path + "/" + meta["name"].(string)
	if s.objects[key] != nil {
		status(409, "AlreadyExists")
		return
	}
	s.creates++
	meta["uid"] = uuid.NewString()
	meta["resourceVersion"] = fmt.Sprint(s.creates)
	s.objects[key] = object
	if s.loseNADReceipt && strings.Contains(r.URL.Path, "network-attachment-definitions") {
		s.loseNADReceipt = false
		status(500, "InternalError")
		return
	}
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(object)
}

func newAutoVMNetwork(t *testing.T) (attachmentFixture, *vmNetworkAPI, string) {
	t.Helper()
	f := newAttachmentFixture(t)
	api := &vmNetworkAPI{legacy: f.api}
	host := httptest.NewServer(api)
	t.Cleanup(host.Close)
	provider, err := data.NewKCProvider(f.p, &rest.Config{Host: host.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	f.provider = provider
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = time.Millisecond
	policy.RetryMin = time.Millisecond
	f.resourceWorker, err = biz.NewWorker(f.p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if err = provider.ConfigureVMAutoNetwork(data.VMAutoNetworkConfig{}); err != nil {
		t.Fatal(err)
	}
	if err = f.p.SetVMNetworkResolver(provider, time.Minute); err != nil {
		t.Fatal(err)
	}
	return f, api, host.URL
}

func awaitAutoVMNetwork(t *testing.T, f attachmentFixture) data.ManagedVMNAD {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := f.resourceWorker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		policy, err := f.provider.LookupManagedVMNetwork(ctx, f.r.TenantID, f.r.VPCID, f.r.SubnetID, f.r.ClusterID, f.r.Namespace)
		if err == nil {
			return policy
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("existing Subnet never received its automatic VM network")
	return data.ManagedVMNAD{}
}

func TestVMAutoNetworkPGBackfillsExistingSubnetAndPinsAcrossRestart(t *testing.T) {
	f, api, host := newAutoVMNetwork(t)
	ctx := context.Background()
	api.loseNADReceipt = true
	policy := awaitAutoVMNetwork(t, f)
	if policy.NADUID == "" || policy.NamespaceUID == "" || policy.NADName != "ani-vm-primary-v1" || policy.KCNSubnetRef == policy.NADName {
		t.Fatal("untrusted or incomplete automatic identity", policy)
	}
	api.mu.Lock()
	creates := api.creates
	api.mu.Unlock()
	if creates != 2 {
		t.Fatal("lost receipt caused duplicate NAD or SA", creates)
	}
	legacy := policy
	legacy.NADName = "legacy-main"
	legacy.NADUID = "legacy-nad-uid"
	if err := f.provider.ConfigureVMNetworks([]data.ManagedVMNAD{legacy}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.provider.LookupManagedVMNetwork(ctx, f.r.TenantID, f.r.VPCID, f.r.SubnetID, f.r.ClusterID, f.r.Namespace); err != nil || got.NADUID != policy.NADUID {
		t.Fatal("static compatibility prevented automatic selection for new VMs", got, err)
	}
	api.mu.Lock()
	legacyKey := "/apis/k8s.cni.cncf.io/v1/namespaces/" + f.r.Namespace + "/network-attachment-definitions/legacy-main"
	autoKey := "/apis/k8s.cni.cncf.io/v1/namespaces/" + f.r.Namespace + "/network-attachment-definitions/ani-vm-primary-v1"
	api.objects[legacyKey] = map[string]any{"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition", "metadata": map[string]any{"namespace": f.r.Namespace, "name": "legacy-main", "uid": "legacy-nad-uid"}, "spec": api.objects[autoKey]["spec"]}
	api.mu.Unlock()
	legacyExpectation := biz.VMNetworkExpectation{NADName: legacy.NADName, NADUID: legacy.NADUID, NADConfigDigest: legacy.NADConfigDigest}
	selection := biz.VMNetworkSelection{TenantID: f.r.TenantID, VPCID: f.r.VPCID, SubnetID: f.r.SubnetID, ClusterID: f.r.ClusterID, Namespace: f.r.Namespace, KCNSubnetRef: policy.KCNSubnetRef}
	if _, err := f.provider.ResolveVMNetwork(ctx, selection, legacyExpectation); err != nil {
		t.Fatal("automatic preparation invalidated frozen static VM identity", err)
	}
	// A new Provider instance must read the durable identity, never reconstruct
	// it by trusting arbitrary observed NADs. Lookup and resolve remain read-only.
	restarted, err := data.NewKCProvider(f.p, &rest.Config{Host: host, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.ConfigureVMAutoNetwork(data.VMAutoNetworkConfig{}); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.LookupManagedVMNetwork(ctx, f.r.TenantID, f.r.VPCID, f.r.SubnetID, f.r.ClusterID, f.r.Namespace)
	if err != nil || got != policy {
		t.Fatal("restart lost managed identity", got, err)
	}
	e := biz.VMNetworkExpectation{NADName: policy.NADName, NADUID: policy.NADUID, NADConfigDigest: policy.NADConfigDigest}
	f.r.WorkloadMode = "vm_managed"
	f.r.VMExpectation = &e
	if _, err = f.p.ResolveVMNetworkSelection(ctx, f.r, time.Minute); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	after := api.creates
	api.mu.Unlock()
	if after != creates {
		t.Fatal("read-only resolver created Kubernetes objects")
	}
	if _, err = restarted.LookupManagedVMNetwork(ctx, uuid.NewString(), f.r.VPCID, f.r.SubnetID, f.r.ClusterID, f.r.Namespace); err == nil {
		t.Fatal("cross-tenant network lookup accepted")
	}
	if _, err = restarted.LookupManagedVMNetwork(ctx, f.r.TenantID, f.r.VPCID, f.r.SubnetID, "other-cluster", f.r.Namespace); err == nil {
		t.Fatal("cross-cluster network lookup accepted")
	}
	// A second Subnet uses the same tenant NAD with its own real KCN binding.
	s, err := f.n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: f.r.TenantID, VPCID: f.r.VPCID, Name: "second", CIDR: "10.42.2.0/24", IdempotencyKey: "second-auto-subnet"})
	if err != nil {
		t.Fatal(err)
	}
	s = driveSubnet(t, f.n, f.resourceWorker, s, biz.Available)
	second, err := restarted.LookupManagedVMNetwork(ctx, f.r.TenantID, f.r.VPCID, s.ID, f.r.ClusterID, f.r.Namespace)
	if err != nil || second.NADUID != policy.NADUID || second.KCNSubnetRef == policy.KCNSubnetRef {
		t.Fatal("Subnet binding or shared NAD identity incorrect", second, err)
	}
	otherTenant := uuid.NewString()
	v := availableVPC(t, f.n, f.resourceWorker, otherTenant, "second-tenant-auto")
	otherSubnet, err := f.n.CreateSubnet(ctx, biz.CreateSubnet{TenantID: otherTenant, VPCID: v.ID, Name: "other-tenant-subnet", CIDR: "10.42.1.0/24", IdempotencyKey: "other-auto-subnet"})
	if err != nil {
		t.Fatal(err)
	}
	otherSubnet = driveSubnet(t, f.n, f.resourceWorker, otherSubnet, biz.Available)
	other, err := restarted.LookupManagedVMNetwork(ctx, otherTenant, v.ID, otherSubnet.ID, f.r.ClusterID, "tenant-"+otherTenant)
	if err != nil || other.Namespace == policy.Namespace || other.NamespaceUID == policy.NamespaceUID || other.NADUID == policy.NADUID {
		t.Fatal("tenant namespaces shared managed identities", other, err)
	}
	// SA replacement is rejected at the read-only admission seam, before the
	// next periodic worker has noticed it or changed its readiness projection.
	api.mu.Lock()
	saKey := "/api/v1/namespaces/" + f.r.Namespace + "/serviceaccounts/ani-vm-workload"
	api.objects[saKey]["metadata"].(map[string]any)["uid"] = "replacement-sa-uid"
	api.mu.Unlock()
	if _, err = f.p.ResolveVMNetworkSelection(ctx, f.r, time.Minute); err == nil {
		t.Fatal("same-name ServiceAccount replacement accepted")
	}
}

func TestVMAutoNetworkPGRefusesForeignObjectsWithoutFailingSubnet(t *testing.T) {
	f, api, _ := newAutoVMNetwork(t)
	ctx := context.Background()
	key := "/apis/k8s.cni.cncf.io/v1/namespaces/" + f.r.Namespace + "/network-attachment-definitions/ani-vm-primary-v1"
	foreign := map[string]any{"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition", "metadata": map[string]any{"name": "ani-vm-primary-v1", "namespace": f.r.Namespace, "uid": "foreign-uid"}, "spec": map[string]any{"config": `{"type":"kc-networking","server_socket":"/foreign.sock"}`}}
	api.objects = map[string]map[string]any{key: foreign}
	for i := 0; i < 12; i++ {
		if _, err := f.resourceWorker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := f.provider.LookupManagedVMNetwork(ctx, f.r.TenantID, f.r.VPCID, f.r.SubnetID, f.r.ClusterID, f.r.Namespace); err == nil {
		t.Fatal("foreign existing NAD was adopted")
	}
	s, err := f.n.GetSubnet(ctx, f.r.TenantID, f.r.SubnetID)
	if err != nil || s.State != biz.Available || s.Reason != "" {
		t.Fatal("NAD failure degraded ordinary network", s, err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.objects[key]["metadata"].(map[string]any)["uid"] != "foreign-uid" || api.creates != 0 {
		t.Fatal("foreign NAD was mutated or replaced")
	}
}

func TestVMAutoNetworkPGPinsPartialReceiptAndRefusesUIDDrift(t *testing.T) {
	f, api, _ := newAutoVMNetwork(t)
	ctx := context.Background()
	api.denySA = true
	var nadUID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := f.resourceWorker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		_ = f.owner.QueryRow(ctx, "SELECT nad_uid FROM network_vm_namespaces WHERE tenant_id=$1 AND cluster_id=$2", f.r.TenantID, f.r.ClusterID).Scan(&nadUID)
		if nadUID != "" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if nadUID == "" {
		t.Fatal("NAD receipt was not persisted before SA failure")
	}
	api.mu.Lock()
	key := "/apis/k8s.cni.cncf.io/v1/namespaces/" + f.r.Namespace + "/network-attachment-definitions/ani-vm-primary-v1"
	api.objects[key]["metadata"].(map[string]any)["uid"] = "replacement-uid"
	api.denySA = false
	api.mu.Unlock()
	for i := 0; i < 12; i++ {
		if _, err := f.resourceWorker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := f.provider.LookupManagedVMNetwork(ctx, f.r.TenantID, f.r.VPCID, f.r.SubnetID, f.r.ClusterID, f.r.Namespace); err == nil {
		t.Fatal("same-name replacement after partial preparation was accepted")
	}
	api.mu.Lock()
	creates := api.creates
	api.mu.Unlock()
	if creates != 1 {
		t.Fatal("UID drift caused a new SA or NAD", creates)
	}
	var persisted string
	if err := f.owner.QueryRow(ctx, "SELECT nad_uid FROM network_vm_namespaces WHERE tenant_id=$1 AND cluster_id=$2", f.r.TenantID, f.r.ClusterID).Scan(&persisted); err != nil || persisted != nadUID {
		t.Fatal("pinned NAD UID changed", persisted, err)
	}
}

func TestVMAutoNetworkPGStopAndClosureSurviveAuxiliaryPreparationFailure(t *testing.T) {
	for _, fault := range []string{"not-ready", "sa-missing", "sa-replaced", "current-template-changed"} {
		t.Run(fault, func(t *testing.T) {
			f, api, _ := newAutoVMNetwork(t)
			policy := awaitAutoVMNetwork(t, f)
			f.r.WorkloadMode = "vm_managed"
			f.r.VMExpectation = &biz.VMNetworkExpectation{NADName: policy.NADName, NADUID: policy.NADUID, NADConfigDigest: policy.NADConfigDigest}
			a := f.prepare(t)
			ctx := context.Background()
			saKey := "/api/v1/namespaces/" + a.Namespace + "/serviceaccounts/ani-vm-workload"
			switch fault {
			case "not-ready":
				if _, err := f.owner.Exec(ctx, "UPDATE network_vm_namespaces SET ready=false,reason='VM_NETWORK_PREPARATION_FAILED' WHERE tenant_id=$1 AND cluster_id=$2", a.TenantID, a.ClusterID); err != nil {
					t.Fatal(err)
				}
			case "sa-missing":
				api.mu.Lock()
				delete(api.objects, saKey)
				api.mu.Unlock()
			case "sa-replaced":
				api.mu.Lock()
				api.objects[saKey]["metadata"].(map[string]any)["uid"] = "replacement-sa"
				api.mu.Unlock()
			case "current-template-changed":
				if err := f.provider.ConfigureVMAutoNetwork(data.VMAutoNetworkConfig{ServerSocket: "/run/reconfigured-kcn.sock"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.p.ResolveVMNetworkSelection(ctx, f.r, time.Minute); err == nil {
				t.Fatal("new/running VM admitted after auxiliary failure", fault)
			}
			c := vmConsumerFor(a)
			c.DesiredRunState = "stopped"
			c.RetiredPods, c.CurrentPods = c.CurrentPods, nil
			setManagedConsumer(f, c)
			workerPolicy := biz.DefaultWorkerPolicy()
			workerPolicy.ObserveEvery = 5 * time.Millisecond
			workerPolicy.RetryMin = time.Millisecond
			var err error
			f.worker, err = biz.NewAttachmentWorker(f.p, f.provider, f.consumer, uuid.NewString(), workerPolicy)
			if err != nil {
				t.Fatal(err)
			}
			a = f.step(t, a.ID)
			if a.State != biz.Reserved || a.Reason != "" || a.PodUID != "" || attachmentProtocolBlocked(t, f, a.ID) {
				t.Fatal("stopped VM could not converge after auxiliary failure", fault, a)
			}
			closed := time.Now()
			c.SnapshotRevision++
			c.State = "closed"
			c.DesiredRunState = "deleted"
			c.FinalizationID = uuid.NewString()
			c.ClosedAt = &closed
			setManagedConsumer(f, c)
			for i := 0; i < 4 && a.State != biz.Released; i++ {
				a = f.step(t, a.ID)
			}
			if a.State != biz.Released || a.Reason != "" || attachmentProtocolBlocked(t, f, a.ID) {
				t.Fatal("closed VM remained fenced on auxiliary failure", fault, a)
			}
			api.mu.Lock()
			creates := api.creates
			api.mu.Unlock()
			if creates != 2 {
				t.Fatal("closure repaired or adopted Kubernetes objects", creates)
			}
		})
	}
}
