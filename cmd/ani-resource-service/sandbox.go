package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v3/transport"
	"github.com/google/uuid"

	sandboxpb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	storagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	storagedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/storage"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/service/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

type sandboxComponents struct {
	repo      *data.Postgres
	service   sandboxpb.SandboxServiceServer
	provider  *data.KubernetesProvider
	worker    *biz.Worker
	execution transport.Server
	retry     *sandboxRetry
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
	if c != nil && c.retry != nil {
		c.retry.Close()
	}
	if c != nil && c.repo != nil {
		c.repo.Close()
	}
}

// Sandbox shares the Resource listener and existing infrastructure. Missing
// dependencies leave its methods explicitly unavailable without disabling
// unrelated Resource services or discarding already persisted work.
func openSandbox(ctx context.Context, network *conf.Network, compute *computeComponents, images *imageComponents, logger *slog.Logger) (*sandboxComponents, error) {
	attempt := func(ctx context.Context) (*sandboxComponents, error) {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return openSandboxOnce(attemptCtx, network, compute, images, logger)
	}
	initial, err := attempt(ctx)
	if err != nil || initial.repo != nil || compute == nil || compute.resolver == nil || network == nil || network.ClusterId != compute.settings.ClusterID {
		return initial, err
	}
	// The other Resource services keep their existing dependencies. Retry only
	// Sandbox's own composition, preserving the same Compute and Image objects.
	retry := newSandboxRetry(func(ctx context.Context) (*sandboxRetryTarget, error) {
		component, err := attempt(ctx)
		if err != nil || component.repo == nil {
			return nil, err
		}
		return &sandboxRetryTarget{service: component.service, execution: component.execution, close: component.Close}, nil
	}, 5*time.Second)
	return &sandboxComponents{service: retry, execution: retry, retry: retry}, nil
}

func openSandboxOnce(ctx context.Context, network *conf.Network, compute *computeComponents, images *imageComponents, logger *slog.Logger) (*sandboxComponents, error) {
	unavailable := func(reason string, err error) (*sandboxComponents, error) {
		if err != nil {
			logger.Warn("sandbox unavailable", "reason", reason, "error", err)
		} else {
			logger.Warn("sandbox unavailable", "reason", reason)
		}
		return &sandboxComponents{service: unavailableSandbox{}}, nil
	}
	if compute == nil || compute.resolver == nil {
		return unavailable("compute is not configured", nil)
	}
	if network == nil {
		return unavailable("shared Kubernetes configuration is missing", nil)
	}
	if network.ClusterId != compute.settings.ClusterID {
		return unavailable("compute and Kubernetes cluster IDs do not match", nil)
	}
	raw, err := readImagePrivateFile(compute.settings.DatabaseDSNFile)
	if err != nil {
		return unavailable("shared database credentials are unavailable", err)
	}
	defer clear(raw)
	repo, err := data.OpenPostgres(ctx, strings.TrimSpace(string(raw)))
	if err != nil {
		return unavailable("sandbox database schema is not ready", err)
	}
	rest, err := clientcmd.BuildConfigFromFlags("", network.Kubeconfig)
	if err != nil {
		repo.Close()
		return unavailable("shared Kubernetes configuration is invalid", err)
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
		return unavailable("Kubernetes client initialization failed", err)
	}
	classes, err := storagedata.NewKubernetes(client)
	if err != nil {
		repo.Close()
		return unavailable("storage client initialization failed", err)
	}
	storageUseCase, err := storagebiz.NewUseCase(classes, nil, storagebiz.Config{NamespacePrefix: network.NamespacePrefix})
	if err != nil {
		repo.Close()
		return unavailable("storage configuration is invalid", err)
	}
	var imageRuntime data.ImageRuntime
	if images != nil {
		imageRuntime = images.runtime
	}
	handler, err := sandbox.NewService(repo, compute.resolver, sandbox.Config{
		ClusterID: compute.settings.ClusterID, NamespacePrefix: network.NamespacePrefix,
		WorkspaceSize: "5Gi", RuntimeClassResolver: data.RuntimeClass{Client: client},
		ImageResolver:        data.Image{Runtime: imageRuntime},
		StorageClassResolver: data.StorageClass{Classes: storageUseCase, Namespaces: classes, Client: client, NamespacePrefix: network.NamespacePrefix},
		NamespaceResolver:    data.TenantNamespace{Client: client, NamespacePrefix: network.NamespacePrefix},
	})
	if err != nil {
		repo.Close()
		return unavailable("sandbox service initialization failed", err)
	}
	provider, err := data.NewKubernetesProvider(client, compute.settings.ClusterID)
	if err != nil {
		repo.Close()
		return unavailable("sandbox provider initialization failed", err)
	}
	if err = provider.ConfigureTenantNamespacePrefix(network.NamespacePrefix); err != nil {
		repo.Close()
		return unavailable("sandbox tenant namespace configuration is invalid", err)
	}
	var pulls sandboxPullMaterial = unavailableSandboxPulls{}
	if images != nil {
		pulls = images.runtime
	}
	if err = provider.ConfigureImages(pulls); err != nil {
		repo.Close()
		return unavailable("sandbox image credentials are unavailable", err)
	}
	worker, err := biz.NewWorker(repo, provider, uuid.NewString(), 45*time.Second)
	if err != nil {
		repo.Close()
		return unavailable("sandbox worker initialization failed", err)
	}
	return &sandboxComponents{repo: repo, service: handler, provider: provider, worker: worker, execution: server.NewSandboxWorkerServer(worker, logger)}, nil
}

type sandboxPullMaterial interface {
	GetTenantPullMaterial(context.Context, string) (imagebiz.PullMaterial, error)
}
type unavailableSandboxPulls struct{}

func (unavailableSandboxPulls) GetTenantPullMaterial(context.Context, string) (imagebiz.PullMaterial, error) {
	return imagebiz.PullMaterial{}, biz.ErrDependencyUnavailable
}

// The listener always registers Sandbox, even while its shared dependencies
// are unavailable. This keeps callers from confusing a missing service with a
// valid empty result and preserves Resource's unrelated modes.
type unavailableSandbox struct {
	sandboxpb.UnimplementedSandboxServiceServer
}

func sandboxUnavailable() error {
	return status.Error(codes.Unavailable, "SANDBOX_DEPENDENCY_UNAVAILABLE")
}
func (unavailableSandbox) ResolveCreateSpec(context.Context, *sandboxpb.ResolveCreateSpecRequest) (*sandboxpb.ResolveCreateSpecResponse, error) {
	return nil, sandboxUnavailable()
}
func (unavailableSandbox) AcceptCreate(context.Context, *sandboxpb.AcceptCreateRequest) (*sandboxpb.AcceptCreateResponse, error) {
	return nil, sandboxUnavailable()
}
func (unavailableSandbox) GetSandbox(context.Context, *sandboxpb.GetSandboxRequest) (*sandboxpb.GetSandboxResponse, error) {
	return nil, sandboxUnavailable()
}
func (unavailableSandbox) ListSandboxes(context.Context, *sandboxpb.ListSandboxesRequest) (*sandboxpb.ListSandboxesResponse, error) {
	return nil, sandboxUnavailable()
}
func (unavailableSandbox) GetOperation(context.Context, *sandboxpb.GetOperationRequest) (*sandboxpb.GetOperationResponse, error) {
	return nil, sandboxUnavailable()
}
func (unavailableSandbox) Pause(context.Context, *sandboxpb.PauseRequest) (*sandboxpb.PauseResponse, error) {
	return nil, sandboxUnavailable()
}
func (unavailableSandbox) Resume(context.Context, *sandboxpb.ResumeRequest) (*sandboxpb.ResumeResponse, error) {
	return nil, sandboxUnavailable()
}
func (unavailableSandbox) Delete(context.Context, *sandboxpb.DeleteRequest) (*sandboxpb.DeleteResponse, error) {
	return nil, sandboxUnavailable()
}
