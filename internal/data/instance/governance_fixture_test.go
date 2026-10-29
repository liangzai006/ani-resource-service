//go:build instanceintegration

package instance_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	quotapb "github.com/zhangzhe-ctrl/ani-governance/api/quota/gen/go/quota/service/v1"
	computepb "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	sandboxpb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	sandboxdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	server "github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	computeservice "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/compute"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/instance"
	sandboxservice "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/sandbox"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
)

type fixtureImage struct{}

func (fixtureImage) Resolve(context.Context, string, string, string) (biz.ResolvedImage, error) {
	digest := "sha256:" + strings.Repeat("a", 64)
	return biz.ResolvedImage{Reference: "registry.invalid/work@" + digest, Digest: digest}, nil
}

type fixtureEnvironment struct{}

func (fixtureEnvironment) ValidateEnvironment(context.Context, string, string, string, string) error {
	return nil
}

// Only Kubernetes/Controller/PVC facts are controlled here. Each instance has
// its own external object state; service admission, PG, references and all
// execution phases continue through the real Resource implementation.
type governanceFixtureProvider struct {
	mu        sync.Mutex
	clusterID string
	instances map[string]*restartGuardBoundary
}

func (p *governanceFixtureProvider) state(i biz.Instance) *restartGuardBoundary {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := i.TenantID + "/" + i.ID
	if p.instances[key] == nil {
		p.instances[key] = newRestartGuardBoundary()
	}
	return p.instances[key]
}

func (p *governanceFixtureProvider) ValidateEnvironment(_ context.Context, tenant, namespace, _, _ string) error {
	if !biz.UUID(tenant) || namespace != "tenant-"+tenant {
		return biz.ErrOwnership
	}
	return nil
}

func (p *governanceFixtureProvider) volumeScope(tenant, cluster, namespace string) bool {
	return biz.UUID(tenant) && cluster == p.clusterID && namespace == "tenant-"+tenant
}

func (p *governanceFixtureProvider) borrowedUID(namespace string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(p.clusterID+"/"+namespace+"/research-data")).String()
}

func (p *governanceFixtureProvider) ResolveVolumes(_ context.Context, tenant, cluster, namespace string, mounts []biz.VolumeMount) ([]biz.VolumeMount, error) {
	if !p.volumeScope(tenant, cluster, namespace) {
		return nil, biz.ErrPVCNotFound
	}
	out := make([]biz.VolumeMount, 0, len(mounts))
	for _, mount := range mounts {
		if mount.PVCName != "research-data" {
			return nil, biz.ErrPVCNotFound
		}
		if mount.ClusterID != "" || mount.Namespace != "" || mount.PVCUID != "" || mount.Ownership != "" || mount.ReferenceGeneration != 0 {
			return nil, biz.ErrInvalid
		}
		mount.ClusterID, mount.Namespace, mount.PVCUID, mount.Ownership = cluster, namespace, p.borrowedUID(namespace), "borrowed"
		out = append(out, mount)
	}
	return out, nil
}

func (p *governanceFixtureProvider) ListMountablePVCs(_ context.Context, tenant, cluster, namespace string, page, size uint32, name string) ([]biz.MountablePVC, int64, error) {
	if !p.volumeScope(tenant, cluster, namespace) {
		return nil, 0, biz.ErrPVCNotFound
	}
	_, _, offset, err := biz.PageWindow(page, size)
	if err != nil {
		return nil, 0, err
	}
	if !strings.Contains("research-data", name) {
		return []biz.MountablePVC{}, 0, nil
	}
	if offset > 0 {
		return []biz.MountablePVC{}, 1, nil
	}
	return []biz.MountablePVC{{PVCName: "research-data", Phase: "Bound", RequestedCapacity: "20Gi", AccessModes: []string{"ReadWriteOnce"}, VolumeMode: "Filesystem", Selectable: true}}, 1, nil
}

func (p *governanceFixtureProvider) VerifyAddedMounts(_ context.Context, i biz.Instance, target []biz.VolumeMount) error {
	for _, mount := range target {
		present := false
		for _, current := range i.CurrentMounts {
			present = present || mount == current
		}
		if !present && (!p.volumeScope(i.TenantID, mount.ClusterID, mount.Namespace) || mount.PVCName != "research-data" || mount.PVCUID != p.borrowedUID(mount.Namespace) || mount.Ownership != "borrowed") {
			return biz.ErrPVCReferenceChanged
		}
	}
	return nil
}

func (p *governanceFixtureProvider) Observe(ctx context.Context, i biz.Instance) (biz.Observation, error) {
	return p.state(i).Observe(ctx, i)
}
func (p *governanceFixtureProvider) ObserveIdentity(ctx context.Context, i biz.Instance) (biz.Observation, error) {
	return p.state(i).ObserveIdentity(ctx, i)
}
func (p *governanceFixtureProvider) VerifyRuntime(ctx context.Context, i biz.Instance, o biz.Observation) (biz.Observation, error) {
	return p.state(i).VerifyRuntime(ctx, i, o)
}
func (p *governanceFixtureProvider) VerifyMountTemplate(ctx context.Context, i biz.Instance, before, target []biz.VolumeMount) (biz.MountTemplateProof, error) {
	return p.state(i).VerifyMountTemplate(ctx, i, before, target)
}
func (p *governanceFixtureProvider) ApplyMutation(ctx context.Context, i biz.Instance, m biz.Mutation) error {
	if m.StoppedTarget == nil {
		return biz.ErrInvalid
	}
	if err := p.state(i).ApplyMutation(ctx, i, m); err != nil {
		return err
	}
	return nil
}
func (p *governanceFixtureProvider) UpdateMounts(ctx context.Context, i biz.Instance, m biz.Mutation, target []biz.VolumeMount) error {
	if err := p.VerifyAddedMounts(ctx, i, target); err != nil {
		return err
	}
	return p.state(i).UpdateMounts(ctx, i, m, target)
}
func (p *governanceFixtureProvider) EnsureWorkspace(ctx context.Context, i biz.Instance) (string, error) {
	return p.state(i).EnsureWorkspace(ctx, i)
}
func (p *governanceFixtureProvider) CreateNotebook(ctx context.Context, i biz.Instance) (string, error) {
	return p.state(i).CreateNotebook(ctx, i)
}
func (p *governanceFixtureProvider) SetStopped(ctx context.Context, i biz.Instance, o biz.Observation, stopped bool) error {
	return p.state(i).SetStopped(ctx, i, o, stopped)
}
func (p *governanceFixtureProvider) DeleteNotebook(ctx context.Context, i biz.Instance, o biz.Observation) error {
	return p.state(i).DeleteNotebook(ctx, i, o)
}
func (p *governanceFixtureProvider) DeleteWorkspace(ctx context.Context, i biz.Instance, o biz.Observation) error {
	return p.state(i).DeleteWorkspace(ctx, i, o)
}

// A subprocess with actual Resource intake, tenant identity, restricted PG and
// lifecycle worker. Image metadata and Kubernetes/Controller/PVC facts are
// controlled external boundaries; this fixture provides no LIVE/GPU evidence.
func TestInstanceGovernanceFixture(t *testing.T) {
	dir := os.Getenv("INSTANCE_JOINT_FIXTURE_DIR")
	if dir == "" {
		t.Fatal("isolated fixture directory required")
	}
	var gpu *acc.GpuRequest
	rawGPU := strings.TrimSpace(os.Getenv("INSTANCE_JOINT_GPU_JSON"))
	if rawGPU != "" && rawGPU != "null" {
		gpu = new(acc.GpuRequest)
		if e := protojson.Unmarshal([]byte(rawGPU), gpu); e != nil {
			t.Fatal(e)
		}
	}
	clusterID := os.Getenv("INSTANCE_JOINT_CLUSTER_ID")
	if clusterID == "" {
		clusterID = "11111111-1111-4111-8111-111111111111"
	}
	f := database(t)
	adminCtx := platformContext()
	preset := compute.Preset{Code: "cpu", Name: "cpu", ComputeType: biz.TypeCPU, CPUMillis: 1000, MemoryMiB: 2048, Enabled: true}
	if gpu != nil {
		clusterID = gpu.ClusterId
		var ref pb.GpuPresetReference
		if raw := os.Getenv("INSTANCE_JOINT_GPU_REFERENCE_JSON"); raw == "" {
			t.Fatal("complete managed GPU reference required")
		} else if e := protojson.Unmarshal([]byte(raw), &ref); e != nil {
			t.Fatal(e)
		}
		preset.Code = "shared"
		preset.Name = "shared"
		preset.ComputeType = biz.TypeGPU
		preset.GPU = &compute.GPUReference{ProfileID: ref.ProfileId, ProfileVersion: ref.ProfileVersion, SpecDigest: ref.SpecDigest, DevicesPerInstance: ref.DevicesPerInstance, ClusterID: ref.ClusterId, PoolID: ref.PoolId, Mode: ref.Mode.String(), ModelKey: ref.ModelKey, MemoryMiBPerDevice: ref.MemoryMibPerDevice, CoreLimitPercent: ref.CoreLimitPercent}
	}
	preset, e := f.presets.CreatePreset(adminCtx, preset)
	if e != nil {
		t.Fatal(e)
	}
	f.repo.UseIntentGate(true)
	providerState := &governanceFixtureProvider{clusterID: clusterID, instances: map[string]*restartGuardBoundary{}}
	cpuQuota := &releaseEndpoint{immediateReceipt: true}
	var reporter *data.Quota
	if gpu == nil {
		reporter, e = data.NewQuota(f.repo, cpuQuota)
		if e != nil {
			t.Fatal(e)
		}
	}
	if path := os.Getenv("INSTANCE_JOINT_QUOTA_FILE"); path != "" {
		var cfg struct{ Address, CAFile, CertFile, KeyFile, ServerName string }
		raw, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(raw, &cfg); e != nil {
			t.Fatal(e)
		}
		pem, e := os.ReadFile(cfg.CAFile)
		if e != nil {
			t.Fatal(e)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			t.Fatal("fixture CA invalid")
		}
		cert, e := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if e != nil {
			t.Fatal(e)
		}
		connection, e := grpc.NewClient(cfg.Address, grpc.WithDisableRetry(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}, ServerName: cfg.ServerName})))
		if e != nil {
			t.Fatal(e)
		}
		defer connection.Close()
		reporter, e = data.NewQuota(f.repo, quotapb.NewQuotaReleaseServiceClient(connection))
		if e != nil {
			t.Fatal(e)
		}
	}
	{
		// Only the external Kubernetes boundary is controlled in this ledger
		// slice. Admission, worker, cleanup decisions, outbox and Gov RPC are real.
		worker, e := biz.NewWorker(f.repo, providerState, "cross-service-worker")
		if e != nil {
			t.Fatal(e)
		}
		execution := server.NewInstanceWorkerServer(worker, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if reporter != nil {
			execution = server.NewInstanceWorkerServer(worker, slog.New(slog.NewTextHandler(io.Discard, nil)), reporter)
		}
		done := make(chan error, 1)
		go func() { done <- execution.Start(context.Background()) }()
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if e := execution.Stop(ctx); e != nil {
				t.Error(e)
			}
			if e := <-done; e != nil {
				t.Error(e)
			}
			if gpu == nil && cpuQuota.calls != 0 {
				t.Error("CPU lifecycle called the GPU quota boundary")
			}
		}()
	}
	handler, e := service.NewService(f.repo, computeResolver(t, f, clusterID), fixtureImage{}, providerState, service.Config{NamespacePrefix: "tenant-", ClusterID: clusterID, ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/sh", UID: 1000, GID: 1000, NewIntentsEnabled: true, RestartEnabled: true, PVCMutationsEnabled: true})
	if e != nil {
		t.Fatal(e)
	}
	identity := testenv.NewInstanceServiceTLS(t)
	reserved, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	terminalAddress := reserved.Addr().String()
	_ = reserved.Close()
	terminal, e := server.NewInstanceTerminalServer(terminalAddress, identity.CAFile, identity.CertFile, identity.KeyFile, service.NewTerminalValidation(f.repo, providerState))
	if e != nil {
		t.Fatal(e)
	}
	terminalDone := make(chan error, 1)
	go func() { terminalDone <- terminal.Start(context.Background()) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := terminal.Stop(ctx); e != nil {
			t.Error(e)
		}
		if e := <-terminalDone; e != nil {
			t.Error(e)
		}
	}()
	tlsConfig, e := server.GovernanceTLS(identity.CAFile, identity.CertFile, identity.KeyFile)
	if e != nil {
		t.Fatal(e)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	transport := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.UnaryInterceptor(server.GovernanceUnary()), grpc.StreamInterceptor(server.GovernanceStreams(false)))
	pb.RegisterInstanceServiceServer(transport, handler)
	catalog, e := computeservice.NewService(f.presets, computeResolver(t, f, clusterID), clusterID)
	if e != nil {
		t.Fatal(e)
	}
	computepb.RegisterComputePresetServiceServer(transport, catalog)
	// The joint fixture owns its database. Register the real Sandbox intake on
	// the same listener; no Kubernetes lifecycle is claimed by this fixture.
	if e = sandboxdata.Migrate(context.Background(), f.ownerDSN, f.runtimeRole); e != nil {
		t.Fatal(e)
	}
	sandboxRepo, e := sandboxdata.OpenPostgres(context.Background(), f.dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer sandboxRepo.Close()
	sandboxHandler, e := sandboxservice.NewService(sandboxRepo, computeResolver(t, f, clusterID), sandboxservice.Config{
		ClusterID: clusterID, NamespacePrefix: "tenant-", RuntimeClass: "sandbox-kata",
		StorageClass: "ani-block", WorkspaceSize: "1Gi", ImagePrefixes: []string{"registry.invalid/"},
		NamespaceResolver: sandboxdata.TenantNamespace{},
	})
	if e != nil {
		t.Fatal(e)
	}
	sandboxpb.RegisterSandboxServiceServer(transport, sandboxHandler)
	done := make(chan error, 1)
	go func() { done <- transport.Serve(listener) }()
	defer func() {
		transport.Stop()
		if e := <-done; e != nil {
			t.Error(e)
		}
	}()
	probe, e := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(identity.Client)))
	if e != nil {
		t.Fatal(e)
	}
	defer probe.Close()
	tenant := uuid.NewString()
	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probeCtx = metadata.NewOutgoingContext(probeCtx, metadata.Pairs("x-ani-tenant-id", tenant, "x-ani-actor", "governance:user:42", "x-ani-request-id", uuid.NewString()))
	if _, e = sandboxpb.NewSandboxServiceClient(probe).ListSandboxes(probeCtx, &sandboxpb.ListSandboxesRequest{TenantId: tenant}); e != nil {
		t.Fatalf("sandbox joint fixture RPC unavailable: %v", e)
	}
	raw, e := json.Marshal(map[string]string{"PresetID": preset.ID, "ClusterID": clusterID, "Address": listener.Addr().String(), "CAFile": identity.CAFile, "CertFile": identity.ClientCertFile, "KeyFile": identity.ClientKeyFile, "TerminalAddress": terminalAddress, "SGClientCertFile": identity.SGClientCertFile, "SGClientKeyFile": identity.SGClientKeyFile, "SGServerCertFile": identity.SGServerCertFile, "SGServerKeyFile": identity.SGServerKeyFile, "GovControlCertFile": identity.GovControlCertFile, "GovControlKeyFile": identity.GovControlKeyFile})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "fixture.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() || scanner.Text() != "stop" {
		t.Fatal("fixture shutdown command required")
	}
}

var _ biz.Provider = (*governanceFixtureProvider)(nil)
var _ biz.LifecycleProvider = (*governanceFixtureProvider)(nil)
var _ biz.VolumeResolver = (*governanceFixtureProvider)(nil)
var _ biz.EnvironmentValidator = (*governanceFixtureProvider)(nil)
