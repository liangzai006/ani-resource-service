package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	sandboxpb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	computebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	computedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/compute"
	instancedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	sandboxdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestSandboxRefusesMismatchedSharedCluster(t *testing.T) {
	compute := &computeComponents{resolver: &computebiz.Resolver{}, settings: computeSettings{ClusterID: "11111111-1111-4111-8111-111111111111"}}
	network := &conf.Network{ClusterId: "22222222-2222-4222-8222-222222222222"}
	components, err := openSandbox(context.Background(), network, compute, nil, newRuntimeLogger(io.Discard))
	if err != nil || components == nil || components.execution != nil {
		t.Fatalf("mismatched clusters should disable Sandbox: components=%v err=%v", components, err)
	}
	_, err = components.service.ListSandboxes(context.Background(), &sandboxpb.ListSandboxesRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("mismatched clusters returned %s, want unavailable", status.Code(err))
	}
}

// Exercise the real composition and authenticated transport with an isolated
// database and a fake Kubernetes endpoint. This does not test the lifecycle or
// the permissions of a real cluster.
func TestBuildAppRegistersSandboxFromSharedConfiguration(t *testing.T) {
	fixture := testenv.NewDatabase(t)
	ctx := context.Background()
	ownerDSN := fixture.Owner.Config().ConnString()
	for _, migrate := range []struct {
		name string
		fn   func(context.Context, string, string) error
	}{
		{"instance history", instancedata.Migrate},
		{"compute", computedata.Migrate},
		{"sandbox", sandboxdata.Migrate},
	} {
		if err := migrate.fn(ctx, ownerDSN, fixture.RuntimeRole); err != nil {
			t.Fatalf("migrate %s fixture: %v", migrate.name, err)
		}
	}

	const clusterID = "11111111-1111-4111-8111-111111111111"
	dir := t.TempDir()
	dsnFile := filepath.Join(dir, "runtime.dsn")
	if err := os.WriteFile(dsnFile, []byte(fixture.RuntimeDSN), 0600); err != nil {
		t.Fatal(err)
	}
	computeConfig, err := json.Marshal(computeSettings{DatabaseDSNFile: dsnFile, ClusterID: clusterID})
	if err != nil {
		t.Fatal(err)
	}
	computeFile := filepath.Join(dir, "compute.json")
	if err = os.WriteFile(computeFile, computeConfig, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANI_COMPUTE_CONFIG_FILE", computeFile)
	t.Setenv("ANI_INSTANCE_CONFIG_FILE", "")
	t.Setenv("ANI_SANDBOX_CONFIG_FILE", "")
	t.Setenv("ANI_VM_CONFIG_FILE", "")

	tlsFixture := testenv.NewServiceTLS(t)
	t.Setenv("ANI_NETWORK_CLIENT_CA", tlsFixture.CAFile)
	t.Setenv("ANI_NETWORK_TLS_CERT", tlsFixture.CertFile)
	t.Setenv("ANI_NETWORK_TLS_KEY", tlsFixture.KeyFile)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fixture Kubernetes endpoint unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)
	grpcAddress, adminAddress := reserveAddress(t), reserveAddress(t)
	for adminAddress == grpcAddress {
		adminAddress = reserveAddress(t)
	}
	bc := &conf.Bootstrap{Network: &conf.Network{
		DatabaseDsn: fixture.RuntimeDSN, Kubeconfig: testenv.Kubeconfig(t, provider.URL),
		ClusterId: clusterID, NamespacePrefix: "tenant-", CursorSigningKey: testenv.SigningKey(),
		Worker: &conf.Worker{Lease: durationpb.New(20 * time.Second), RequestTimeout: durationpb.New(5 * time.Second), ObserveEvery: durationpb.New(10 * time.Second), StaleAfter: durationpb.New(time.Minute), RetryMin: durationpb.New(time.Second), RetryMax: durationpb.New(time.Minute), PollInterval: durationpb.New(100 * time.Millisecond)},
	}, Server: &conf.Server{
		Grpc:            &conf.Server_GRPC{Network: "tcp", Addr: grpcAddress, Timeout: durationpb.New(time.Second)},
		Admin:           &conf.Server_Admin{Network: "tcp", Addr: adminAddress, Timeout: durationpb.New(time.Second)},
		ShutdownTimeout: durationpb.New(3 * time.Second),
	}}
	app, err := buildApp(bc, newRuntimeLogger(io.Discard))
	if err != nil {
		t.Fatalf("build application with shared Resource configuration: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	t.Cleanup(func() {
		if err := app.Stop(); err != nil {
			t.Errorf("stop application: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run application: %v", err)
			}
		case <-time.After(4 * time.Second):
			t.Error("application did not stop")
		}
	})
	waitForHTTP(t, "http://"+adminAddress+"/readyz")
	conn, err := grpc.NewClient(grpcAddress, grpc.WithTransportCredentials(credentials.NewTLS(tlsFixture.Client)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	tenant := uuid.NewString()
	requestCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, metadata.Pairs(
		"x-ani-tenant-id", tenant, "x-ani-actor", "governance:user:42", "x-ani-request-id", uuid.NewString(),
	)), 3*time.Second)
	defer cancel()
	client := sandboxpb.NewSandboxServiceClient(conn)
	if _, err = client.ListSandboxes(requestCtx, &sandboxpb.ListSandboxesRequest{TenantId: tenant}); err != nil {
		t.Fatalf("Sandbox RPC was not registered with shared configuration: %v", err)
	}
	_, err = client.ListSandboxes(requestCtx, &sandboxpb.ListSandboxesRequest{TenantId: uuid.NewString()})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-tenant Sandbox list status = %s, want permission denied", status.Code(err))
	}
}

// Sandbox can be installed after the shared Resource process has started. The
// test exercises the real migration check, retry composition, mTLS listener,
// and worker lifecycle with an isolated PostgreSQL database and fake K8s.
func TestBuildAppRecoversSandboxAfterMigration(t *testing.T) {
	fixture := testenv.NewDatabase(t)
	ctx := context.Background()
	ownerDSN := fixture.Owner.Config().ConnString()
	for _, migrate := range []struct {
		name string
		fn   func(context.Context, string, string) error
	}{
		{"instance history", instancedata.Migrate},
		{"compute", computedata.Migrate},
	} {
		if err := migrate.fn(ctx, ownerDSN, fixture.RuntimeRole); err != nil {
			t.Fatalf("migrate %s fixture: %v", migrate.name, err)
		}
	}

	const clusterID = "11111111-1111-4111-8111-111111111111"
	dir := t.TempDir()
	dsnFile := filepath.Join(dir, "runtime.dsn")
	if err := os.WriteFile(dsnFile, []byte(fixture.RuntimeDSN), 0600); err != nil {
		t.Fatal(err)
	}
	computeConfig, err := json.Marshal(computeSettings{DatabaseDSNFile: dsnFile, ClusterID: clusterID})
	if err != nil {
		t.Fatal(err)
	}
	computeFile := filepath.Join(dir, "compute.json")
	if err = os.WriteFile(computeFile, computeConfig, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANI_COMPUTE_CONFIG_FILE", computeFile)
	t.Setenv("ANI_INSTANCE_CONFIG_FILE", "")
	t.Setenv("ANI_SANDBOX_CONFIG_FILE", "")
	t.Setenv("ANI_VM_CONFIG_FILE", "")

	tlsFixture := testenv.NewServiceTLS(t)
	t.Setenv("ANI_NETWORK_CLIENT_CA", tlsFixture.CAFile)
	t.Setenv("ANI_NETWORK_TLS_CERT", tlsFixture.CertFile)
	t.Setenv("ANI_NETWORK_TLS_KEY", tlsFixture.KeyFile)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fixture Kubernetes endpoint unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)
	grpcAddress, adminAddress := reserveAddress(t), reserveAddress(t)
	for adminAddress == grpcAddress {
		adminAddress = reserveAddress(t)
	}
	bc := &conf.Bootstrap{Network: &conf.Network{
		DatabaseDsn: fixture.RuntimeDSN, Kubeconfig: testenv.Kubeconfig(t, provider.URL),
		ClusterId: clusterID, NamespacePrefix: "tenant-", CursorSigningKey: testenv.SigningKey(),
		Worker: &conf.Worker{Lease: durationpb.New(20 * time.Second), RequestTimeout: durationpb.New(5 * time.Second), ObserveEvery: durationpb.New(10 * time.Second), StaleAfter: durationpb.New(time.Minute), RetryMin: durationpb.New(time.Second), RetryMax: durationpb.New(time.Minute), PollInterval: durationpb.New(100 * time.Millisecond)},
	}, Server: &conf.Server{
		Grpc:            &conf.Server_GRPC{Network: "tcp", Addr: grpcAddress, Timeout: durationpb.New(time.Second)},
		Admin:           &conf.Server_Admin{Network: "tcp", Addr: adminAddress, Timeout: durationpb.New(time.Second)},
		ShutdownTimeout: durationpb.New(3 * time.Second),
	}}
	app, err := buildApp(bc, newRuntimeLogger(io.Discard))
	if err != nil {
		t.Fatalf("build application before Sandbox migration: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	t.Cleanup(func() {
		if err := app.Stop(); err != nil {
			t.Errorf("stop application: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run application: %v", err)
			}
		case <-time.After(4 * time.Second):
			t.Error("application did not stop")
		}
	})
	waitForHTTP(t, "http://"+adminAddress+"/readyz")
	conn, err := grpc.NewClient(grpcAddress, grpc.WithTransportCredentials(credentials.NewTLS(tlsFixture.Client)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	tenant := uuid.NewString()
	requestCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs(
		"x-ani-tenant-id", tenant, "x-ani-actor", "governance:user:42", "x-ani-request-id", uuid.NewString(),
	))
	client := sandboxpb.NewSandboxServiceClient(conn)
	call := func() error {
		callCtx, cancel := context.WithTimeout(requestCtx, 2*time.Second)
		defer cancel()
		_, err := client.ListSandboxes(callCtx, &sandboxpb.ListSandboxesRequest{TenantId: tenant})
		return err
	}
	if err = call(); status.Code(err) != codes.Unavailable {
		t.Fatalf("before migration Sandbox RPC status = %s, want unavailable: %v", status.Code(err), err)
	}
	if err = sandboxdata.Migrate(ctx, ownerDSN, fixture.RuntimeRole); err != nil {
		t.Fatalf("migrate Sandbox while application is running: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		err = call()
		if err == nil {
			break
		}
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("unexpected Sandbox RPC status while recovering: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox did not recover after migration: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
