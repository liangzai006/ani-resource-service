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
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
)

func buildApp(bc *conf.Bootstrap, logger *slog.Logger) (*kratos.App, error) {
	if err := bc.Validate(); err != nil {
		return nil, err
	}
	tlsConfig, err := server.GovernanceTLS(os.Getenv("ANI_NETWORK_CLIENT_CA"), os.Getenv("ANI_NETWORK_TLS_CERT"), os.Getenv("ANI_NETWORK_TLS_KEY"))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repository, err := data.OpenPostgres(ctx, bc.Network.DatabaseDsn, data.Placement{ClusterID: bc.Network.ClusterId, NamespacePrefix: bc.Network.NamespacePrefix})
	if err != nil {
		return nil, err
	}
	images, err := openImage(context.Background(), bc.Image)
	if err != nil {
		repository.Close()
		return nil, err
	}
	compute, err := openCompute(context.Background())
	if err != nil {
		images.Close()
		repository.Close()
		return nil, err
	}
	instances, err := openInstance(context.Background(), bc.Network, images, compute, logger)
	if err != nil {
		compute.Close()
		images.Close()
		repository.Close()
		return nil, err
	}
	sandboxes, err := openSandbox(context.Background(), bc.Network, compute, images, logger)
	if err != nil {
		instances.Close()
		compute.Close()
		images.Close()
		repository.Close()
		return nil, err
	}
	configured := false
	defer func() {
		if !configured {
			sandboxes.Close()
			instances.Close()
			compute.Close()
			images.Close()
			repository.Close()
		}
	}()
	provider, observation, err := openNetworkProvider(repository, bc.Network)
	if err != nil {
		return nil, err
	}
	key, _ := base64.StdEncoding.DecodeString(bc.Network.CursorSigningKey)
	if err = repository.ConfigureVPCCIDRPresets(bc.Network.VpcCidrPresets, provider); err != nil {
		return nil, err
	}
	w := bc.Network.Worker
	policy := biz.WorkerPolicy{Lease: w.Lease.AsDuration(), RequestTimeout: w.RequestTimeout.AsDuration(), ObserveEvery: w.ObserveEvery.AsDuration(), StaleAfter: w.StaleAfter.AsDuration(), RetryMin: w.RetryMin.AsDuration(), RetryMax: w.RetryMax.AsDuration()}
	repository.UseBaseConnectivityFreshness(policy.StaleAfter)
	network, err := biz.NewNetwork(repository, key, policy.StaleAfter, time.Now)
	if err != nil {
		return nil, err
	}
	var execution *server.WorkerServer
	var storageRecovery *server.StorageDeleteWorker
	readiness := server.NewReadiness(func() bool { return execution != nil && execution.Ready() }, func() bool { return storageRecovery == nil || storageRecovery.Ready() })
	observability, err := server.NewObservability(Name, Version, readiness)
	if err != nil {
		return nil, err
	}
	if err = observability.RegisterObservation(observation); err != nil {
		return nil, err
	}
	if instances != nil {
		if err = observability.RegisterStorageDeleteMetrics(instances.repo.PVCDeleteStats); err != nil {
			return nil, err
		}
	}
	worker, err := biz.NewWorker(repository, provider, uuid.NewString(), policy, func(ctx context.Context, work biz.Work, progress biz.Progress, err error) {
		observability.ObserveWork(ctx, logger, work, progress, err)
	})
	if err != nil {
		_ = observability.Shutdown(ctx)
		return nil, err
	}
	consumer, err := data.NewInstanceConsumer(bc.Network.InstanceConsumerEndpoint)
	if err != nil {
		_ = observability.Shutdown(ctx)
		return nil, err
	}
	attachments := biz.NewAttachments(repository, policy.StaleAfter)
	routedConsumer, err := wireInstanceNetwork(instances, network, attachments, consumer, bc.Network.ClusterId, bc.Network.NamespacePrefix, policy.StaleAfter)
	if err != nil {
		consumer.Close()
		_ = observability.Shutdown(ctx)
		return nil, err
	}
	vms, routedConsumer, err := wireVM(context.Background(), bc.Network, compute, repository, provider, attachments, instances, routedConsumer, policy.StaleAfter)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !configured {
			vms.Close()
		}
	}()
	attachmentWorker, err := biz.NewAttachmentWorker(repository, provider, routedConsumer, uuid.NewString(), policy, observability.ObserveAttachmentWork)
	if err != nil {
		consumer.Close()
		_ = observability.Shutdown(ctx)
		return nil, err
	}
	defer func() {
		if !configured {
			consumer.Close()
		}
	}()
	switchWorker, err := biz.NewVirtualSwitchWorker(repository, provider, uuid.NewString(), policy, func(ctx context.Context, work biz.VirtualSwitchWork, progress biz.VirtualSwitchProgress, err error) {
		observability.ObserveVirtualSwitchWork(ctx, logger, work, progress, err)
	})
	if err != nil {
		return nil, err
	}
	retirementWorker, err := biz.NewLegacyDeviceRetirementStepper(repository, provider, uuid.NewString(), policy)
	if err != nil {
		return nil, err
	}
	defaultService, defaultWorker, err := wireDefaultVPCEgress(repository, provider, bc.Network, policy)
	if err != nil {
		return nil, err
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
			return nil, err
		}
	}
	middlewares := observability.ServerMiddleware(logger)
	grpcServer, err := server.NewGRPCServer(bc.Server.Grpc, readiness, tlsConfig, false, middlewares...)
	if err != nil {
		return nil, err
	}
	storageComponents, err := openStorage(ctx, bc, instances)
	if err != nil {
		return nil, err
	}
	storagev1.RegisterStorageServiceServer(grpcServer, storageComponents.service)
	storageRecovery = storageComponents.worker
	networkv1.RegisterNetworkServiceServer(grpcServer, service.NewNetworkService(network, attachments))
	repository.UseEgressInfrastructure(provider)
	egress, err := biz.NewEgress(repository, provider, biz.ContextEgressAuthorization{}, key, policy.StaleAfter, time.Now)
	if err != nil {
		return nil, err
	}
	networkv1.RegisterTenantEgressServiceServer(grpcServer, service.NewTenantEgressService(egress))
	networkv1.RegisterPlatformNetworkServiceServer(grpcServer, service.NewPlatformNetworkService(egress))
	networkv1.RegisterPlatformEIPServiceServer(grpcServer, service.NewPlatformEIPService(egress))
	networkv1.RegisterPlatformDeviceRetirementServiceServer(grpcServer, service.NewPlatformDeviceRetirementService(egress))
	if defaultService != nil {
		networkv1.RegisterPlatformDefaultEgressServiceServer(grpcServer, defaultService)
	}
	if lb := bc.Network.LoadBalancer; lb != nil && lb.EnableIsolatedApi {
		lbs, err := biz.NewLoadBalancers(repository, biz.ContextEgressAuthorization{}, key, policy.StaleAfter, time.Now)
		if err != nil {
			return nil, err
		}
		networkv1.RegisterTenantLoadBalancerServiceServer(grpcServer, service.NewTenantLoadBalancerService(lbs))
	}
	if images != nil {
		imagev1.RegisterTenantImageServiceServer(grpcServer, images.tenant)
	}
	if compute != nil {
		computev1.RegisterComputePresetServiceServer(grpcServer, compute.service)
	}
	if instances != nil {
		instancev1.RegisterInstanceServiceServer(grpcServer, instances.service)
	}
	if sandboxes != nil {
		sandboxv1.RegisterSandboxServiceServer(grpcServer, sandboxes.service)
	}
	if vms != nil {
		vmv1.RegisterVMServiceServer(grpcServer, vms.service)
	}
	adminServer := server.NewAdminServer(bc.Server.Admin, readiness, observability.Gatherer(), middlewares...)
	servers := []transport.Server{grpcServer, adminServer, observation, execution}
	if storageComponents.worker != nil {
		servers = append(servers, storageComponents.worker)
	}
	imageRuntime, err := openImageRuntime(images, middlewares...)
	if err != nil {
		return nil, err
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
	app := kratos.New(
		kratos.ID(id), kratos.Name(Name), kratos.Version(Version), kratos.Logger(logger),
		kratos.Server(servers...),
		kratos.AfterStart(func(context.Context) error { readiness.Set(true); return nil }),
		kratos.BeforeStop(func(context.Context) error { readiness.Set(false); return nil }),
		kratos.AfterStop(func(ctx context.Context) error {
			sandboxes.Close()
			vms.Close()
			consumer.Close()
			instances.Close()
			compute.Close()
			images.Close()
			repository.Close()
			return observability.Shutdown(ctx)
		}),
		kratos.StopTimeout(bc.Server.ShutdownTimeout.AsDuration()),
	)
	configured = true
	return app, nil
}
