package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"time"

	kratos "github.com/go-kratos/kratos/v3"
	"github.com/go-kratos/kratos/v3/transport"
	"github.com/google/uuid"
	computev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	instancev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	sandboxv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	storagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/storage/v1"
	vmv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	networkService "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
)

// Explicit governance composition: full worker so tenant writes are executed,
// with tenant and platform services registered. Exact method and trusted scope
// authorization gate every call before business code. It never
// falls back to the legacy listener.
func runGovernance(bc *conf.Bootstrap, logger *slog.Logger) error {
	if err := bc.Validate(); err != nil {
		return err
	}
	tlsConfig, err := server.GovernanceTLS(os.Getenv("ANI_NETWORK_CLIENT_CA"), os.Getenv("ANI_NETWORK_TLS_CERT"), os.Getenv("ANI_NETWORK_TLS_KEY"))
	if err != nil {
		return err
	}
	images, err := openImage(context.Background(), bc.Image)
	if err != nil {
		return err
	}
	defer images.Close()
	compute, err := openCompute(context.Background())
	if err != nil {
		return err
	}
	defer compute.Close()
	instances, err := openInstance(context.Background(), bc.Network, images, compute, logger)
	if err != nil {
		return err
	}
	defer instances.Close()
	sandboxes, err := openSandbox(context.Background(), bc.Network, compute, images, logger)
	if err != nil {
		return err
	}
	defer sandboxes.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	repository, err := data.OpenPostgres(ctx, bc.Network.DatabaseDsn, data.Placement{ClusterID: bc.Network.ClusterId, NamespacePrefix: bc.Network.NamespacePrefix})
	cancel()
	if err != nil {
		return err
	}
	configured := false
	defer func() {
		if !configured {
			repository.Close()
		}
	}()
	// Writes need the same provider access as full mode; the governance
	// boundary is enforced by service registration and the allowlist, not by
	// removing the worker.
	provider, observation, err := openNetworkProvider(repository, bc.Network)
	if err != nil {
		return err
	}
	if err = repository.ConfigureVPCCIDRPresets(bc.Network.VpcCidrPresets, provider); err != nil {
		return err
	}
	w := bc.Network.Worker
	policy := biz.WorkerPolicy{Lease: w.Lease.AsDuration(), RequestTimeout: w.RequestTimeout.AsDuration(), ObserveEvery: w.ObserveEvery.AsDuration(), StaleAfter: w.StaleAfter.AsDuration(), RetryMin: w.RetryMin.AsDuration(), RetryMax: w.RetryMax.AsDuration()}
	repository.UseBaseConnectivityFreshness(policy.StaleAfter)
	repository.UseEgressInfrastructure(provider)
	key, _ := base64.StdEncoding.DecodeString(bc.Network.CursorSigningKey)
	networkUC, err := biz.NewNetwork(repository, key, policy.StaleAfter, time.Now)
	if err != nil {
		return err
	}
	var execution *server.WorkerServer
	var storageRecovery *server.StorageDeleteWorker
	readiness := server.NewReadiness(func() bool { return execution != nil && execution.Ready() }, func() bool { return storageRecovery == nil || storageRecovery.Ready() })
	observability, err := server.NewObservability(Name, Version, readiness)
	if err != nil {
		return err
	}
	if instances != nil {
		if err = observability.RegisterStorageDeleteMetrics(instances.repo.PVCDeleteStats); err != nil {
			return err
		}
	}
	defer func() {
		if err := observability.Shutdown(context.Background()); err != nil {
			logger.Error("observability shutdown", "error", err)
		}
	}()
	if err = observability.RegisterObservation(observation); err != nil {
		return err
	}
	worker, err := biz.NewWorker(repository, provider, uuid.NewString(), policy, func(ctx context.Context, work biz.Work, progress biz.Progress, err error) {
		observability.ObserveWork(ctx, logger, work, progress, err)
	})
	if err != nil {
		return err
	}
	egress, err := biz.NewEgress(repository, provider, biz.ContextEgressAuthorization{}, key, policy.StaleAfter, time.Now)
	if err != nil {
		return err
	}
	lbs, err := biz.NewLoadBalancers(repository, biz.ContextEgressAuthorization{}, key, policy.StaleAfter, time.Now)
	if err != nil {
		return err
	}
	attachments := biz.NewAttachments(repository, policy.StaleAfter)
	fallback, err := data.NewInstanceConsumer(bc.Network.InstanceConsumerEndpoint)
	if err != nil {
		return err
	}
	defer fallback.Close()
	consumer, err := wireInstanceNetwork(instances, networkUC, attachments, fallback, bc.Network.ClusterId, bc.Network.NamespacePrefix, policy.StaleAfter)
	if err != nil {
		return err
	}
	vms, consumer, err := wireVM(context.Background(), bc.Network, compute, repository, provider, attachments, instances, consumer, policy.StaleAfter)
	if err != nil {
		return err
	}
	defer vms.Close()
	attachmentWorker, err := biz.NewAttachmentWorker(repository, provider, consumer, uuid.NewString(), policy, observability.ObserveAttachmentWork)
	if err != nil {
		return err
	}
	switchWorker, err := biz.NewVirtualSwitchWorker(repository, provider, uuid.NewString(), policy, func(ctx context.Context, work biz.VirtualSwitchWork, progress biz.VirtualSwitchProgress, err error) {
		observability.ObserveVirtualSwitchWork(ctx, logger, work, progress, err)
	})
	if err != nil {
		return err
	}
	retirementWorker, err := biz.NewLegacyDeviceRetirementStepper(repository, provider, uuid.NewString(), policy)
	if err != nil {
		return err
	}
	defaultService, defaultWorker, err := wireDefaultVPCEgress(repository, provider, bc.Network, policy)
	if err != nil {
		return err
	}
	additional := []server.Stepper{attachmentWorker, switchWorker, retirementWorker}
	if vms != nil {
		additional = append(additional, vms.worker)
	}
	if defaultWorker != nil {
		additional = append(additional, defaultWorker)
	}
	execution = server.NewWorkerServer(worker, repository, logger, w.PollInterval.AsDuration(), additional...)
	if o := bc.Network.Observation; o != nil {
		if err = execution.SetConcurrency(int(o.WorkersPerKind)); err != nil {
			return err
		}
	}
	middlewares := observability.ServerMiddleware(logger)
	gs, err := server.NewGRPCServer(bc.Server.Grpc, readiness, tlsConfig, false, middlewares...)
	if err != nil {
		return err
	}
	networkv1.RegisterNetworkServiceServer(gs, networkService.NewNetworkService(networkUC, attachments))
	networkv1.RegisterTenantEgressServiceServer(gs, networkService.NewTenantEgressService(egress))
	networkv1.RegisterPlatformNetworkServiceServer(gs, networkService.NewPlatformNetworkService(egress))
	networkv1.RegisterPlatformEIPServiceServer(gs, networkService.NewPlatformEIPService(egress))
	networkv1.RegisterPlatformDeviceRetirementServiceServer(gs, networkService.NewPlatformDeviceRetirementService(egress))
	if defaultService != nil {
		networkv1.RegisterPlatformDefaultEgressServiceServer(gs, defaultService)
	}
	networkv1.RegisterTenantLoadBalancerServiceServer(gs, networkService.NewTenantLoadBalancerService(lbs))
	if images != nil {
		imagev1.RegisterTenantImageServiceServer(gs, images.tenant)
	}
	if compute != nil {
		computev1.RegisterComputePresetServiceServer(gs, compute.service)
	}
	if instances != nil {
		instancev1.RegisterInstanceServiceServer(gs, instances.service)
	}
	if sandboxes != nil {
		sandboxv1.RegisterSandboxServiceServer(gs, sandboxes.service)
	}
	if vms != nil {
		vmv1.RegisterVMServiceServer(gs, vms.service)
	}
	storageComponents, err := openStorage(context.Background(), bc, instances)
	if err != nil {
		return err
	}
	storagev1.RegisterStorageServiceServer(gs, storageComponents.service)
	storageRecovery = storageComponents.worker
	configured = true
	admin := server.NewAdminServer(bc.Server.Admin, readiness, observability.Gatherer(), middlewares...)
	servers := []transport.Server{gs, admin, observation, execution}
	if storageComponents.worker != nil {
		servers = append(servers, storageComponents.worker)
	}
	imageRuntime, err := openImageRuntime(images, middlewares...)
	if err != nil {
		return err
	}
	if imageRuntime != nil {
		servers = append(servers, imageRuntime)
	}
	if sandboxes != nil && sandboxes.execution != nil {
		servers = append(servers, sandboxes.execution)
	}
	if vms != nil && vms.console != nil {
		servers = append(servers, vms.console)
	}
	if instances != nil {
		servers = append(servers, instances.execution, instances.terminal)
	}
	return kratos.New(kratos.ID(id), kratos.Name(Name), kratos.Version(Version), kratos.Logger(logger),
		kratos.Server(servers...),
		kratos.AfterStart(func(context.Context) error { readiness.Set(true); return nil }),
		kratos.BeforeStop(func(context.Context) error { readiness.Set(false); return nil }),
		kratos.AfterStop(func(ctx context.Context) error {
			repository.Close()
			return nil
		}),
		kratos.StopTimeout(bc.Server.ShutdownTimeout.AsDuration())).Run()
}
