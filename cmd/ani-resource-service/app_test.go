package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	sandboxpb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
)

func TestBuildAppRunsProductionComposition(t *testing.T) {
	t.Setenv("ANI_COMPUTE_CONFIG_FILE", "")
	t.Setenv("ANI_SANDBOX_CONFIG_FILE", "")
	t.Setenv("ANI_INSTANCE_CONFIG_FILE", "")
	tlsFixture := testenv.NewServiceTLS(t)
	t.Setenv("ANI_NETWORK_CLIENT_CA", tlsFixture.CAFile)
	t.Setenv("ANI_NETWORK_TLS_CERT", tlsFixture.CertFile)
	t.Setenv("ANI_NETWORK_TLS_KEY", tlsFixture.KeyFile)
	fixture := testenv.NewDatabase(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer provider.Close()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	body := "apiVersion: v1\nkind: Config\ncurrent-context: test\nclusters:\n- name: test\n  cluster:\n    server: " + provider.URL + "\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\nusers:\n- name: test\n  user: {}\n"
	if err := os.WriteFile(kubeconfig, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	grpcAddress := reserveAddress(t)
	adminAddress := reserveAddress(t)
	for adminAddress == grpcAddress {
		adminAddress = reserveAddress(t)
	}
	bc := &conf.Bootstrap{Network: &conf.Network{DatabaseDsn: fixture.RuntimeDSN, Kubeconfig: kubeconfig, ClusterId: "test-cluster", NamespacePrefix: "tenant-", CursorSigningKey: testenv.SigningKey(), Worker: &conf.Worker{
		Lease: durationpb.New(20 * time.Second), RequestTimeout: durationpb.New(5 * time.Second), ObserveEvery: durationpb.New(10 * time.Second), StaleAfter: durationpb.New(time.Minute), RetryMin: durationpb.New(time.Second), RetryMax: durationpb.New(time.Minute), PollInterval: durationpb.New(100 * time.Millisecond),
	}}, Server: &conf.Server{
		Grpc:            &conf.Server_GRPC{Network: "tcp", Addr: grpcAddress, Timeout: durationpb.New(time.Second)},
		Admin:           &conf.Server_Admin{Network: "tcp", Addr: adminAddress, Timeout: durationpb.New(time.Second)},
		ShutdownTimeout: durationpb.New(3 * time.Second),
	}}
	var logs bytes.Buffer
	app, err := buildApp(bc, newRuntimeLogger(&logs))
	if err != nil {
		t.Fatalf("buildApp() error = %v", err)
	}

	runResult := make(chan error, 1)
	go func() { runResult <- app.Run() }()
	waitForHTTP(t, "http://"+adminAddress+"/readyz")
	assertProductionAdmin(t, adminAddress)
	assertProductionGRPCBoundary(t, grpcAddress, tlsFixture.Client)
	assertSandboxUnavailableWithoutCompute(t, grpcAddress, tlsFixture.Client)

	if err := app.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("production composition did not stop within its timeout")
	}
	if !strings.Contains(logs.String(), `"service.name":"ani-resource-service"`) {
		t.Fatalf("structured service identity missing from logs: %s", logs.String())
	}
	if !regexp.MustCompile(`"trace_id":"[0-9a-f]{32}"`).MatchString(logs.String()) {
		t.Fatalf("request trace correlation missing from logs: %s", logs.String())
	}
	if !regexp.MustCompile(`"span_id":"[0-9a-f]{16}"`).MatchString(logs.String()) {
		t.Fatalf("request span correlation missing from logs: %s", logs.String())
	}
}

func assertSandboxUnavailableWithoutCompute(t *testing.T, address string, clientTLS *tls.Config) {
	t.Helper()
	const tenant = "11111111-1111-4111-8111-111111111111"
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-ani-tenant-id", tenant, "x-ani-actor", "governance:user:7", "x-ani-request-id", "22222222-2222-4222-8222-222222222222")), time.Second)
	defer cancel()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, err = sandboxpb.NewSandboxServiceClient(connection).ListSandboxes(ctx, &sandboxpb.ListSandboxesRequest{TenantId: tenant})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("Sandbox without compute status = %s, want unavailable: %v", status.Code(err), err)
	}
}

func TestBuildAppRejectsInvalidConfig(t *testing.T) {
	_, err := buildApp(&conf.Bootstrap{}, newRuntimeLogger(io.Discard))
	if err == nil {
		t.Fatal("buildApp() unexpectedly accepted an incomplete config")
	}
}

func reserveAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved address: %v", err)
	}
	return address
}

func waitForHTTP(t *testing.T, endpoint string) {
	t.Helper()
	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(endpoint)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("endpoint did not become ready: %s", endpoint)
}

func assertProductionAdmin(t *testing.T, address string) {
	t.Helper()
	response, err := (&http.Client{Timeout: time.Second}).Get("http://" + address + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read /healthz: %v", err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("GET /healthz = %d %q", response.StatusCode, body)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("GET /healthz content type = %q", contentType)
	}
}

// Admin owns health checks. A business listener rejects both plaintext and
// an authenticated service's unregistered health method.
func assertProductionGRPCBoundary(t *testing.T, address string, clientTLS *tls.Config) {
	t.Helper()
	for _, test := range []struct {
		name        string
		credentials credentials.TransportCredentials
		want        codes.Code
	}{
		{"plaintext", insecure.NewCredentials(), codes.Unavailable},
		{"unregistered-health", credentials.NewTLS(clientTLS), codes.PermissionDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-ani-tenant-id", "11111111-1111-4111-8111-111111111111", "x-ani-actor", "governance:user:7", "x-ani-request-id", "22222222-2222-4222-8222-222222222222")), time.Second)
			defer cancel()
			connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(test.credentials))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_, err = grpc_health_v1.NewHealthClient(connection).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
			if status.Code(err) != test.want {
				t.Fatalf("health status=%s, want=%s: %v", status.Code(err), test.want, err)
			}
		})
	}
}
