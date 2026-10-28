package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/service/sandbox"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

type sandboxSettings struct {
	DatabaseDSNFile, Kubeconfig, ClusterID, Namespace, RuntimeClass, StorageClass, WorkspaceSize string
	ImagePrefixes                                                                                []string
}
type sandboxComponents struct {
	repo      *data.Postgres
	service   *sandbox.Service
	provider  *data.KubernetesProvider
	worker    *biz.Worker
	execution *server.SandboxWorkerServer
	settings  sandboxSettings
}

func runSandboxMigration() error {
	dsn, err := readImagePrivateFile(os.Getenv("ANI_SANDBOX_OWNER_DSN_FILE"))
	if err != nil {
		return err
	}
	defer clear(dsn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return data.Migrate(ctx, strings.TrimSpace(string(dsn)), os.Getenv("ANI_SANDBOX_RUNTIME_ROLE"))
}

func (c *sandboxComponents) Close() {
	if c != nil && c.repo != nil {
		c.repo.Close()
	}
}

// Sandbox is independently enabled and talks only to its own repository and
// the in-process ComputeResolver supplied by the Resource composition root.
func openSandbox(ctx context.Context, compute *computeComponents, logger *slog.Logger) (*sandboxComponents, error) {
	path := os.Getenv("ANI_SANDBOX_CONFIG_FILE")
	if path == "" {
		return nil, nil
	}
	body, err := readImagePrivateFile(path)
	if err != nil {
		return nil, err
	}
	defer clear(body)
	var cfg sandboxSettings
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err = d.Decode(&cfg); err != nil || d.Decode(new(any)) != io.EOF || !filepath.IsAbs(cfg.DatabaseDSNFile) || !filepath.IsAbs(cfg.Kubeconfig) || !biz.UUID(cfg.ClusterID) || cfg.Namespace == "" || cfg.RuntimeClass == "" || cfg.StorageClass == "" || cfg.WorkspaceSize == "" || len(cfg.ImagePrefixes) == 0 {
		return nil, fmt.Errorf("invalid sandbox configuration")
	}
	if compute == nil || filepath.Clean(cfg.DatabaseDSNFile) != filepath.Clean(compute.settings.DatabaseDSNFile) || cfg.ClusterID != compute.settings.ClusterID {
		return nil, fmt.Errorf("sandbox requires compute configured with the same managed database and cluster")
	}
	raw, err := readImagePrivateFile(cfg.DatabaseDSNFile)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	repo, err := data.OpenPostgres(ctx, strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, err
	}
	handler, err := sandbox.NewService(repo, compute.resolver, sandbox.Config{ClusterID: cfg.ClusterID, Namespace: cfg.Namespace, RuntimeClass: cfg.RuntimeClass, StorageClass: cfg.StorageClass, WorkspaceSize: cfg.WorkspaceSize, ImagePrefixes: cfg.ImagePrefixes})
	if err != nil {
		repo.Close()
		return nil, err
	}
	rest, err := clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	if err != nil {
		repo.Close()
		return nil, err
	}
	// A worker step may observe PVC+Pod and then mutate one child. Keep each
	// Kubernetes call bounded below the 45s lease budget.
	rest.Timeout = 10 * time.Second
	if rest.QPS == 0 {
		rest.QPS = 20
	}
	if rest.Burst == 0 {
		rest.Burst = 40
	}
	client, err := dynamic.NewForConfig(rest)
	if err != nil {
		repo.Close()
		return nil, err
	}
	provider, err := data.NewKubernetesProvider(client, cfg.ClusterID)
	if err != nil {
		repo.Close()
		return nil, err
	}
	worker, err := biz.NewWorker(repo, provider, uuid.NewString(), 45*time.Second)
	if err != nil {
		repo.Close()
		return nil, err
	}
	return &sandboxComponents{repo: repo, service: handler, provider: provider, worker: worker, execution: server.NewSandboxWorkerServer(worker, logger), settings: cfg}, nil
}
