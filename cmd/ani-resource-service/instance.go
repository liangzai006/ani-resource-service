package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	quota "github.com/zhangzhe-ctrl/ani-governance/api/quota/gen/go/quota/service/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	computedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/compute"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/instance"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

type instanceSettings struct {
	DatabaseDSNFile, SigningKeyFile                                         string
	QuotaAddress, QuotaCAFile, QuotaCertFile, QuotaKeyFile, QuotaServerName string
	// Deprecated: accepted for old private configurations, never used at runtime.
	ReleaseAddress, ReleaseCAFile, ReleaseCertFile, ReleaseKeyFile, ReleaseServerName string
	TerminalAddress                                                                   string
	Service                                                                           service.Config
}
type instanceComponents struct {
	service         *service.Service
	worker          *biz.Worker
	execution       *server.InstanceWorkerServer
	terminal        *server.InstanceTerminalServer
	repo            *data.Postgres
	quotaConnection *grpc.ClientConn
}

func runInstanceMigration() error {
	dsn, e := readImagePrivateFile(os.Getenv("ANI_INSTANCE_OWNER_DSN_FILE"))
	if e != nil {
		return e
	}
	defer clear(dsn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ownerDSN, role := strings.TrimSpace(string(dsn)), os.Getenv("ANI_INSTANCE_RUNTIME_ROLE")
	if e = data.Migrate(ctx, ownerDSN, role); e != nil {
		return e
	}
	return computedata.Migrate(ctx, ownerDSN, role)
}

func runComputeRestore() error {
	dsn, e := readImagePrivateFile(os.Getenv("ANI_INSTANCE_OWNER_DSN_FILE"))
	if e != nil {
		return e
	}
	defer clear(dsn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return computedata.Restore(ctx, strings.TrimSpace(string(dsn)), os.Getenv("ANI_INSTANCE_RUNTIME_ROLE"))
}
func (c *instanceComponents) Close() {
	if c == nil {
		return
	}
	if c.quotaConnection != nil {
		_ = c.quotaConnection.Close()
	}
	if c.repo != nil {
		c.repo.Close()
	}
}
func openInstance(ctx context.Context, network *conf.Network, images *imageComponents, compute *computeComponents, logger *slog.Logger) (*instanceComponents, error) {
	path := os.Getenv("ANI_INSTANCE_CONFIG_FILE")
	if path == "" {
		return nil, nil
	}
	if images == nil || images.runtime == nil {
		return nil, fmt.Errorf("instance requires enabled Image runtime")
	}
	body, e := readImagePrivateFile(path)
	if e != nil {
		return nil, e
	}
	defer clear(body)
	var cfg instanceSettings
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&cfg); e != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("invalid instance configuration")
	}
	if e = requireInstanceCompute(compute, cfg); e != nil {
		return nil, e
	}
	// Namespace configuration is shared by value only, never Network tables.
	cfg.Service.NamespacePrefix = network.NamespacePrefix
	cfg.Service.NewIntentsEnabled = os.Getenv("ANI_INSTANCE_NEW_INTENTS_ENABLED") == "true"
	cfg.Service.RestartEnabled = os.Getenv("ANI_INSTANCE_RESTART_ENABLED") == "true"
	cfg.Service.PVCMutationsEnabled = os.Getenv("ANI_INSTANCE_PVC_MUTATIONS_ENABLED") == "true"
	if e = validateInstanceSettings(cfg); e != nil {
		return nil, e
	}
	dsn, e := readImagePrivateFile(cfg.DatabaseDSNFile)
	if e != nil {
		return nil, e
	}
	defer clear(dsn)
	repo, e := data.OpenPostgres(ctx, strings.TrimSpace(string(dsn)))
	if e != nil {
		return nil, e
	}
	repo.UseIntentGate(cfg.Service.NewIntentsEnabled)
	c := &instanceComponents{repo: repo}
	ready := false
	defer func() {
		if !ready {
			c.Close()
		}
	}()
	signing, e := readImagePrivateFile(cfg.SigningKeyFile)
	if e != nil {
		return nil, e
	}
	defer clear(signing)
	if len(signing) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("instance signing key must contain 64 raw bytes")
	}
	kube, e := clientcmd.BuildConfigFromFlags("", network.Kubeconfig)
	if e != nil {
		return nil, fmt.Errorf("instance Kubernetes configuration invalid")
	}
	kube.QPS = 5
	kube.Burst = 10
	kube.Timeout = 15 * time.Second
	client, e := dynamic.NewForConfig(kube)
	if e != nil {
		return nil, e
	}
	provider, e := data.NewNotebook(client)
	if e != nil {
		return nil, e
	}
	if e = provider.ConfigureClusterID(cfg.Service.ClusterID); e != nil {
		return nil, e
	}
	if e = provider.ConfigureLogs(kube, cfg.Service.LogLimits); e != nil {
		return nil, e
	}
	if e = provider.ConfigureSigningKey(ed25519.PrivateKey(signing)); e != nil {
		return nil, e
	}
	if e = provider.ConfigureImages(images.runtime); e != nil {
		return nil, e
	}
	connection, e := instanceClientConnection(cfg.QuotaAddress, cfg.QuotaCAFile, cfg.QuotaCertFile, cfg.QuotaKeyFile, cfg.QuotaServerName)
	if e != nil {
		return nil, e
	}
	c.quotaConnection = connection
	reporter, e := data.NewQuota(repo, quota.NewQuotaReleaseServiceClient(connection))
	if e != nil {
		return nil, e
	}
	worker, e := biz.NewWorker(repo, provider, uuid.NewString())
	if e != nil {
		return nil, e
	}
	handler, e := service.NewService(repo, compute.resolver, data.Image{Runtime: images.runtime}, provider, cfg.Service)
	if e != nil {
		return nil, e
	}
	c.service = handler
	c.worker = worker
	c.execution = server.NewInstanceWorkerServer(worker, logger, reporter)
	c.terminal, e = server.NewInstanceTerminalServer(cfg.TerminalAddress, os.Getenv("ANI_NETWORK_CLIENT_CA"), os.Getenv("ANI_NETWORK_TLS_CERT"), os.Getenv("ANI_NETWORK_TLS_KEY"), service.NewTerminalValidation(repo, provider))
	if e != nil {
		return nil, e
	}
	ready = true
	return c, nil
}

func validateInstanceSettings(cfg instanceSettings) error {
	for _, f := range []string{cfg.DatabaseDSNFile, cfg.SigningKeyFile, cfg.QuotaCAFile, cfg.QuotaCertFile, cfg.QuotaKeyFile} {
		if !filepath.IsAbs(f) {
			return fmt.Errorf("instance requires absolute managed private paths")
		}
	}
	if cfg.QuotaAddress == "" || cfg.QuotaServerName == "" || strings.ContainsAny(cfg.QuotaServerName, "*:/") {
		return fmt.Errorf("instance quota address and exact server identity required")
	}
	return nil
}

func instanceClientConnection(address, caFile, certFile, keyFile, serverName string) (*grpc.ClientConn, error) {
	if address == "" || serverName == "" || strings.ContainsAny(serverName, "*:/") {
		return nil, fmt.Errorf("instance exact peer identity required")
	}
	ca, e := os.ReadFile(caFile)
	if e != nil {
		return nil, fmt.Errorf("instance peer CA unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("instance peer CA invalid")
	}
	cert, e := tls.LoadX509KeyPair(certFile, keyFile)
	if e != nil {
		return nil, fmt.Errorf("instance peer client identity unavailable")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}, ServerName: serverName, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("instance peer unverified")
		}
		for _, name := range cs.PeerCertificates[0].DNSNames {
			if name == serverName && !strings.Contains(name, "*") {
				return nil
			}
		}
		return fmt.Errorf("instance peer exact DNS SAN mismatch")
	}}
	return grpc.NewClient(address, grpc.WithDisableServiceConfig(), grpc.WithDisableRetry(), grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
}

// Cross-domain wiring stays in the composition root and uses only public use
// cases plus the Instance read port. Existing external consumers stay routed.
func wireInstanceNetwork(c *instanceComponents, network *networkbiz.Network, attachments *networkbiz.Attachments, fallback networkbiz.InstanceConsumer, clusterID, prefix string, freshness time.Duration) (networkbiz.InstanceConsumer, error) {
	if c == nil {
		return fallback, nil
	}
	adapter, err := data.NewInstanceNetwork(network, attachments, clusterID, prefix, freshness)
	if err != nil {
		return nil, err
	}
	if err = c.worker.UseNetwork(adapter); err != nil {
		return nil, err
	}
	if err = c.service.UseNetwork(adapter); err != nil {
		return nil, err
	}
	return data.NewInstanceNetworkConsumer(c.repo, fallback)
}
