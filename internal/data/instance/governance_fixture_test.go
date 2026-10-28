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
	"testing"
	"time"

	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	quotapb "github.com/zhangzhe-ctrl/ani-governance/api/quota/gen/go/quota/service/v1"
	computepb "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	server "github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	computeservice "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/compute"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/instance"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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

// A subprocess with actual Resource intake, tenant identity and restricted PG.
// It verifies the cross-service durable ACK slice only. Image metadata and
// external environment availability are controlled; no GPU/cleanup is claimed.
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
	providerState := &provider{}
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
	if reporter != nil {
		// Only the external Kubernetes boundary is controlled in this ledger
		// slice. Admission, worker, cleanup decisions, outbox and Gov RPC are real.
		worker, e := biz.NewWorker(f.repo, providerState, "cross-service-worker")
		if e != nil {
			t.Fatal(e)
		}
		execution := server.NewInstanceWorkerServer(worker, slog.New(slog.NewTextHandler(io.Discard, nil)), reporter)
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
	handler, e := service.NewService(f.repo, computeResolver(t, f, clusterID), fixtureImage{}, fixtureEnvironment{}, service.Config{NamespacePrefix: "tenant-", ClusterID: clusterID, ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/sh", UID: 1000, GID: 1000})
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
	done := make(chan error, 1)
	go func() { done <- transport.Serve(listener) }()
	defer func() {
		transport.Stop()
		if e := <-done; e != nil {
			t.Error(e)
		}
	}()
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
