package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-kratos/kratos/contrib/otel/v3/tracing"
	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
	"github.com/go-kratos/kratos/v3/log"
	"go.uber.org/automaxprocs/maxprocs"

	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
)

// Name and Version can be overridden with -ldflags at build time.
var (
	Name                = "ani-resource-service"
	Version             = "dev"
	flagconf            string
	flagMigrate         bool
	flagImageMigrate    bool
	flagInstanceMigrate bool
	flagSandboxMigrate  bool
	flagComputeRestore  bool
	flagVMMigrate       bool
	flagNodeFacts       bool
	id, _               = os.Hostname()
)

func init() {
	flag.StringVar(&flagconf, "conf", "configs", "config path, for example -conf configs/config.yaml")
	flag.BoolVar(&flagNodeFacts, "node-facts", false, "run the separately authorized read-only node facts collector")
	flag.BoolVar(&flagMigrate, "migrate", false, "apply Network migrations using explicit owner environment")
	flag.BoolVar(&flagImageMigrate, "image-migrate", false, "apply Image migrations using explicit owner secret file")
	flag.BoolVar(&flagInstanceMigrate, "instance-migrate", false, "apply Instance migrations using explicit owner secret file")
	flag.BoolVar(&flagSandboxMigrate, "sandbox-migrate", false, "apply Sandbox migrations using explicit owner secret file")
	flag.BoolVar(&flagVMMigrate, "vm-migrate", false, "apply VM migrations using explicit owner secret file")
	flag.BoolVar(&flagComputeRestore, "compute-restore", false, "restore pre-compute metadata with all readers stopped using explicit owner secret file")
}

func main() {
	flag.Parse()
	logOutput := os.Stdout
	if imageAdminAction != "" {
		// Keep the operator's single JSON result readable on stdout.
		logOutput = os.Stderr
	}
	logger := newRuntimeLogger(logOutput)
	log.SetDefault(logger)
	execute := func() error {
		if sandboxReviewMode {
			if flagVMMigrate || flagComputeRestore || flagInstanceMigrate || flagSandboxMigrate || flagMigrate || flagImageMigrate || flagNodeFacts || imageAdminAction != "" || baseConnectivityAction != "" {
				return fmt.Errorf("sandbox diagnostic is exclusive with other command modes")
			}
			return runSandboxUnknownReview()
		}
		if flagVMMigrate {
			if flagComputeRestore || flagInstanceMigrate || flagSandboxMigrate || flagMigrate || flagImageMigrate || flagNodeFacts || imageAdminAction != "" || baseConnectivityAction != "" {
				return fmt.Errorf("VM migration is exclusive with other command modes")
			}
			return runVMMigration()
		}
		if flagComputeRestore {
			if flagInstanceMigrate || flagSandboxMigrate || flagMigrate || flagImageMigrate || flagNodeFacts || imageAdminAction != "" || baseConnectivityAction != "" {
				return fmt.Errorf("compute recovery is exclusive with other command modes")
			}
			return runComputeRestore()
		}
		if flagInstanceMigrate {
			if flagSandboxMigrate || flagMigrate || flagImageMigrate || flagNodeFacts || imageAdminAction != "" || baseConnectivityAction != "" {
				return fmt.Errorf("instance migration is exclusive with other command modes")
			}
			return runInstanceMigration()
		}
		if flagSandboxMigrate {
			if flagMigrate || flagImageMigrate || flagNodeFacts || imageAdminAction != "" || baseConnectivityAction != "" {
				return fmt.Errorf("sandbox migration is exclusive with other command modes")
			}
			return runSandboxMigration()
		}
		if err := validateImageAdminModes(); err != nil {
			return err
		}
		if imageAdminAction != "" {
			return runImageAdmin()
		}
		if flagImageMigrate {
			if flagMigrate || flagNodeFacts || baseConnectivityAction != "" {
				return fmt.Errorf("Image migration, Network migration, node facts and base connectivity modes are exclusive")
			}
			return runImageMigration()
		}
		if baseConnectivityAction != "" {
			if flagNodeFacts || flagMigrate {
				return fmt.Errorf("base connectivity, node facts and migration modes are exclusive")
			}
			return runBaseConnectivityCommand()
		}
		if flagNodeFacts {
			if flagMigrate {
				return fmt.Errorf("node facts and migration modes are exclusive")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
			defer cancel()
			return data.RunNodeFactsCollector(ctx, logger)
		}
		if flagMigrate {
			return runMigration()
		}
		return run(logger)
	}
	if err := execute(); err != nil {
		logger.Error("service terminated", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	undoMaxProcs, err := maxprocs.Set(maxprocs.Logger(func(format string, args ...interface{}) {
		log.Info("runtime CPU quota", "detail", fmt.Sprintf(format, args...))
	}))
	if err != nil {
		return fmt.Errorf("configure runtime CPU quota: %w", err)
	}
	defer undoMaxProcs()

	c := config.New(config.WithSource(
		file.NewSource(flagconf),
		env.NewSource("ANI"),
	))
	defer c.Close()
	if err := c.Load(); err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	var bc conf.Bootstrap
	if err := c.Scan(&bc); err != nil {
		return fmt.Errorf("scan config: %w", err)
	}
	switch mode := os.Getenv("ANI_NETWORK_MODE"); mode {
	case "", "full":
		// The full composition uses the same trusted mTLS listener and method
		// authorization as governance and wires the local Instance consumer.
	case "vpc-read":
		return runVPCRead(&bc, logger)
	case "governance":
		return runGovernance(&bc, logger)
	case "compute":
		return runComputeOnly(&bc, logger)
	default:
		return fmt.Errorf("unknown ANI_NETWORK_MODE %q", mode)
	}
	app, err := buildApp(&bc, logger)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}
	if err := app.Run(); err != nil {
		return fmt.Errorf("run app: %w", err)
	}
	return nil
}

func newRuntimeLogger(writer io.Writer) *slog.Logger {
	handler := log.NewHandler(
		log.WithWriter(writer),
		log.WithFormat(log.FormatJSON),
		log.WithLevel(log.LevelInfo),
		log.WithAddSource(true),
		log.WithExtractor(tracing.TraceAttrs),
		log.WithFilter(log.FilterKey(
			"args",
			"authorization",
			"cookie",
			"credential",
			"password",
			"private_key",
			"set-cookie",
			"token",
		)),
	)
	return slog.New(handler).With(
		slog.String("service.id", id),
		slog.String("service.name", Name),
		slog.String("service.version", Version),
	)
}
