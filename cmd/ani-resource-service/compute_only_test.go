package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	computev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	computedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/compute"
	instancedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

func computeOnlyConfig(t *testing.T) *conf.Bootstrap {
	t.Helper()
	grpcAddress, adminAddress := reserveAddress(t), reserveAddress(t)
	for adminAddress == grpcAddress {
		adminAddress = reserveAddress(t)
	}
	return &conf.Bootstrap{Server: &conf.Server{
		Grpc:            &conf.Server_GRPC{Network: "tcp", Addr: grpcAddress, Timeout: durationpb.New(3 * time.Second)},
		Admin:           &conf.Server_Admin{Network: "tcp", Addr: adminAddress, Timeout: durationpb.New(time.Second)},
		ShutdownTimeout: durationpb.New(3 * time.Second),
	}}
}

func TestComputeOnlyModeRequiresTrustedTransport(t *testing.T) {
	bc := computeOnlyConfig(t)
	body, err := protojson.Marshal(bc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	original := flagconf
	t.Cleanup(func() { flagconf = original })
	flagconf = path
	t.Setenv("ANI_NETWORK_MODE", "compute")
	t.Setenv("ANI_NETWORK_CLIENT_CA", "")
	t.Setenv("ANI_NETWORK_TLS_CERT", "")
	t.Setenv("ANI_NETWORK_TLS_KEY", "")
	if err = run(newRuntimeLogger(io.Discard)); err == nil || !strings.Contains(err.Error(), "read governance CA") {
		t.Fatal("compute mode did not use the trusted transport guard without Network", err)
	}
}

func TestComputeOnlyModeRequiresExplicitCatalog(t *testing.T) {
	tlsFixture := testenv.NewServiceTLS(t)
	t.Setenv("ANI_NETWORK_CLIENT_CA", tlsFixture.CAFile)
	t.Setenv("ANI_NETWORK_TLS_CERT", tlsFixture.CertFile)
	t.Setenv("ANI_NETWORK_TLS_KEY", tlsFixture.KeyFile)
	t.Setenv("ANI_COMPUTE_CONFIG_FILE", "")
	app, closeApp, err := buildComputeOnlyApp(computeOnlyConfig(t), newRuntimeLogger(io.Discard))
	if err == nil || !strings.Contains(err.Error(), "requires ANI_COMPUTE_CONFIG_FILE") || app != nil || closeApp != nil {
		t.Fatal("compute mode accepted an absent catalog", err)
	}
}

// This runs the actual composition with real PostgreSQL and mTLS. The test
// fixture's pre-existing Network tables are never configured in this app; no
// Kubernetes fixture or credentials are provided to the composition.
func TestComputeOnlyCatalogAuthorizationAndDatabaseReadiness(t *testing.T) {
	fixture := testenv.NewDatabase(t)
	ctx := context.Background()
	ownerDSN := fixture.Owner.Config().ConnString()
	if err := instancedata.Migrate(ctx, ownerDSN, fixture.RuntimeRole); err != nil {
		t.Fatal("migrate immutable historical schema", err)
	}
	if err := computedata.Migrate(ctx, ownerDSN, fixture.RuntimeRole); err != nil {
		t.Fatal("migrate actual compute catalog", err)
	}
	// Match the compute delivery profile: the historical tables remain, while
	// its runtime has no need to inspect or mutate the Instance domain.
	runtimeRole := pgx.Identifier{fixture.RuntimeRole}.Sanitize()
	if _, err := fixture.Owner.Exec(ctx, "REVOKE ALL ON ALL TABLES IN SCHEMA instance FROM "+runtimeRole+"; REVOKE ALL ON SCHEMA instance FROM "+runtimeRole); err != nil {
		t.Fatal("restrict compute-only runtime", err)
	}
	runtimeConnection, err := pgx.Connect(ctx, fixture.RuntimeDSN)
	if err != nil {
		t.Fatal("connect restricted fixture runtime", err)
	}
	t.Cleanup(func() { _ = runtimeConnection.Close(context.Background()) })
	for _, statement := range []string{
		"SELECT * FROM instance.instances LIMIT 1",
		"CREATE SCHEMA compute_only_forbidden",
		"CREATE TEMP TABLE compute_only_forbidden(id integer)",
	} {
		_, err = runtimeConnection.Exec(ctx, statement)
		var denied *pgconn.PgError
		if !errors.As(err, &denied) || denied.Code != "42501" {
			t.Fatal("compute-only runtime exceeded its database privileges", err)
		}
	}
	dir := t.TempDir()
	dsnFile := filepath.Join(dir, "runtime.dsn")
	if err := os.WriteFile(dsnFile, []byte(fixture.RuntimeDSN), 0600); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(computeSettings{DatabaseDSNFile: dsnFile, ClusterID: "11111111-1111-4111-8111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "compute.json")
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANI_COMPUTE_CONFIG_FILE", path)
	for _, name := range []string{"ANI_INSTANCE_CONFIG_FILE", "ANI_VM_CONFIG_FILE", "ANI_SANDBOX_CONFIG_FILE", "ANI_NETWORK_KUBECONFIG"} {
		t.Setenv(name, filepath.Join(dir, "must-not-open"))
	}
	tlsFixture := testenv.NewInstanceServiceTLS(t)
	t.Setenv("ANI_NETWORK_CLIENT_CA", tlsFixture.CAFile)
	t.Setenv("ANI_NETWORK_TLS_CERT", tlsFixture.CertFile)
	t.Setenv("ANI_NETWORK_TLS_KEY", tlsFixture.KeyFile)
	bc := computeOnlyConfig(t)
	app, closeApp, err := buildComputeOnlyApp(bc, newRuntimeLogger(io.Discard))
	if err != nil {
		t.Fatal("build compute without Network or workload configuration", err)
	}
	t.Cleanup(closeApp)
	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	t.Cleanup(func() {
		if err := app.Stop(); err != nil {
			t.Error("stop compute application", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error("run compute application", err)
			}
		case <-time.After(4 * time.Second):
			t.Error("compute application did not stop")
		}
	})
	origin := "http://" + bc.Server.Admin.Addr
	waitForHTTP(t, origin+"/readyz")
	assertProductionGRPCBoundary(t, bc.Server.Grpc.Addr, tlsFixture.Client)
	connection, err := grpc.NewClient(bc.Server.Grpc.Addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsFixture.Client)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := computev1.NewComputePresetServiceClient(connection)
	platform, cancelPlatform := context.WithTimeout(metadata.NewOutgoingContext(ctx, metadata.Pairs(
		"x-ani-scope", "platform", "x-ani-actor", "governance:user:42", "x-ani-request-id", uuid.NewString(),
	)), 10*time.Second)
	defer cancelPlatform()
	enabled := true
	preset, err := client.AdminCreatePreset(platform, &computev1.AdminCreatePresetRequest{Data: &computev1.PresetData{
		Code: "cpu-inference", Name: "CPU inference", ComputeType: "cpu", CpuMillis: 2000, MemoryMib: 2048, Enabled: &enabled,
	}})
	if err != nil || preset.GetId() == "" {
		t.Fatal("create actual CPU preset through authenticated platform RPC", err)
	}
	tenant := uuid.NewString()
	request, cancelRequest := context.WithTimeout(metadata.NewOutgoingContext(ctx, metadata.Pairs(
		"x-ani-tenant-id", tenant, "x-ani-actor", "governance:user:42", "x-ani-request-id", uuid.NewString(),
	)), 10*time.Second)
	defer cancelRequest()
	list, err := client.ListPresets(request, &computev1.ListPresetsRequest{TenantId: tenant, Usage: computev1.Usage_INFERENCE, ComputeType: "cpu"})
	if err != nil || list.GetTotal() != 1 || len(list.GetItems()) != 1 || list.Items[0].Id != preset.Id {
		t.Fatal("list actual CPU catalog", err)
	}
	selection, err := client.ResolveComputePreset(request, &computev1.ResolveComputePresetRequest{TenantId: tenant, PresetId: preset.Id, Usage: computev1.Usage_INFERENCE})
	if err != nil || selection.GetCpuMillis() != 2000 || selection.GetMemoryMib() != 2048 || selection.GetPresetId() != preset.Id {
		t.Fatal("resolve actual CPU preset", err)
	}
	digest, err := computev1.SelectionDigest(selection)
	if err != nil || selection.SelectionDigest != digest {
		t.Fatal("selection digest differs from frozen catalog values", err)
	}
	if _, err = client.ResolveComputePreset(request, &computev1.ResolveComputePresetRequest{TenantId: uuid.NewString(), PresetId: preset.Id, Usage: computev1.Usage_INFERENCE}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("cross-tenant compute request accepted", err)
	}
	if _, err = client.AdminListPresets(request, &computev1.AdminListPresetsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("tenant delegated platform privileges", err)
	}
	if _, err = client.ListPresets(request, &computev1.ListPresetsRequest{TenantId: tenant}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("unspecified compute usage accepted", err)
	}
	for _, method := range []string{"/network.v1.NetworkService/GetVPC", "/storage.v1.StorageService/ListStorageClasses", "/instance.v1.InstanceService/ListInstances", "/sandbox.v1.SandboxService/ListSandboxes"} {
		if err = connection.Invoke(request, method, &emptypb.Empty{}, &emptypb.Empty{}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("unregistered domain %s returned %s, want permission denied", method, status.Code(err))
		}
	}
	wrongCert, err := tls.LoadX509KeyPair(tlsFixture.SGClientCertFile, tlsFixture.SGClientKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	wrongTLS := tlsFixture.Client.Clone()
	wrongTLS.Certificates = []tls.Certificate{wrongCert}
	wrongClient, err := grpc.NewClient(bc.Server.Grpc.Addr, grpc.WithTransportCredentials(credentials.NewTLS(wrongTLS)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wrongClient.Close() })
	if _, err = computev1.NewComputePresetServiceClient(wrongClient).ListPresets(request, &computev1.ListPresetsRequest{TenantId: tenant, Usage: computev1.Usage_INFERENCE}); status.Code(err) != codes.Unavailable {
		t.Fatal("non-Governance certificate reached compute", err)
	}
	var checksum string
	if err = fixture.Owner.QueryRow(ctx, "SELECT checksum FROM compute.schema_version WHERE version=1").Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.Owner.Exec(ctx, "UPDATE compute.schema_version SET checksum=$1 WHERE version=1", strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	assertComputeHTTPStatus(t, origin+"/readyz", http.StatusServiceUnavailable)
	assertComputeHTTPStatus(t, origin+"/healthz", http.StatusOK)
	if _, err = fixture.Owner.Exec(ctx, "UPDATE compute.schema_version SET checksum=$1 WHERE version=1", checksum); err != nil {
		t.Fatal(err)
	}
	waitForHTTP(t, origin+"/readyz")
	// The catalog lookup no longer needs Instance USAGE, but still rejects an
	// obsolete relation reappearing alongside the current managed catalog.
	if _, err = fixture.Owner.Exec(ctx, "CREATE TABLE instance.compute_presets(id integer)"); err != nil {
		t.Fatal(err)
	}
	assertComputeHTTPStatus(t, origin+"/readyz", http.StatusServiceUnavailable)
	if _, err = fixture.Owner.Exec(ctx, "DROP TABLE instance.compute_presets"); err != nil {
		t.Fatal(err)
	}
	waitForHTTP(t, origin+"/readyz")
}

func assertComputeHTTPStatus(t *testing.T, endpoint string, expected int) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		t.Fatal("read compute health endpoint", err)
	}
	defer response.Body.Close()
	if response.StatusCode != expected {
		t.Fatalf("GET %s returned %d, want %d", endpoint, response.StatusCode, expected)
	}
}
