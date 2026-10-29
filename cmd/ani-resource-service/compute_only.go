package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	kratos "github.com/go-kratos/kratos/v3"
	computev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
)

// Compute has no workload or network provider dependency. Its explicit
// composition exposes the real catalog through the existing trusted business
// transport, without creating Network workers or Kubernetes clients.
func runComputeOnly(bc *conf.Bootstrap, logger *slog.Logger) error {
	app, closeApp, err := buildComputeOnlyApp(bc, logger)
	if err != nil {
		return err
	}
	defer closeApp()
	return app.Run()
}

func buildComputeOnlyApp(bc *conf.Bootstrap, logger *slog.Logger) (*kratos.App, func(), error) {
	if err := bc.ValidateServer(); err != nil {
		return nil, nil, err
	}
	tlsConfig, err := server.GovernanceTLS(os.Getenv("ANI_NETWORK_CLIENT_CA"), os.Getenv("ANI_NETWORK_TLS_CERT"), os.Getenv("ANI_NETWORK_TLS_KEY"))
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	compute, err := openCompute(ctx)
	cancel()
	if err != nil {
		return nil, nil, err
	}
	if compute == nil {
		return nil, nil, fmt.Errorf("compute mode requires ANI_COMPUTE_CONFIG_FILE")
	}
	readiness := server.NewReadiness(func() bool {
		// Each probe checks the actual managed catalog, rather than keeping the
		// successful startup result after PostgreSQL or its schema becomes stale.
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		return compute.repo.CheckReady(ctx) == nil
	})
	obs, err := server.NewObservability(Name, Version, readiness)
	if err != nil {
		compute.Close()
		return nil, nil, err
	}
	var once sync.Once
	closeApp := func() {
		once.Do(func() {
			readiness.Set(false)
			compute.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := obs.Shutdown(ctx); err != nil {
				logger.Error("compute observability shutdown", "error", err)
			}
		})
	}
	middlewares := obs.ServerMiddleware(logger)
	grpcServer, err := server.NewGRPCServer(bc.Server.Grpc, readiness, tlsConfig, false, middlewares...)
	if err != nil {
		closeApp()
		return nil, nil, err
	}
	computev1.RegisterComputePresetServiceServer(grpcServer, compute.service)
	admin := server.NewAdminServer(bc.Server.Admin, readiness, obs.Gatherer(), middlewares...)
	app := kratos.New(
		kratos.ID(id), kratos.Name(Name), kratos.Version(Version), kratos.Logger(logger),
		kratos.Server(grpcServer, admin),
		kratos.AfterStart(func(context.Context) error { readiness.Set(true); return nil }),
		kratos.BeforeStop(func(context.Context) error { readiness.Set(false); return nil }),
		kratos.StopTimeout(bc.Server.ShutdownTimeout.AsDuration()),
	)
	return app, closeApp, nil
}
