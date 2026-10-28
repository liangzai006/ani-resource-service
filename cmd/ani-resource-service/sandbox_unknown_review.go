package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

var sandboxReviewMode bool
var sandboxReviewDSNFile, sandboxReviewKubeconfig, sandboxReviewCluster, sandboxReviewTenant, sandboxReviewID string

func init() {
	flag.BoolVar(&sandboxReviewMode, "sandbox-review-unknown-create", false, "produce a read-only unknown Sandbox create diagnostic")
	flag.StringVar(&sandboxReviewDSNFile, "sandbox-review-dsn-file", "", "private Sandbox database DSN file")
	flag.StringVar(&sandboxReviewKubeconfig, "sandbox-review-kubeconfig", "", "operator Kubernetes credentials")
	flag.StringVar(&sandboxReviewCluster, "sandbox-review-cluster-id", "", "configured logical Sandbox cluster UUID")
	flag.StringVar(&sandboxReviewTenant, "sandbox-review-tenant-id", "", "Sandbox tenant UUID")
	flag.StringVar(&sandboxReviewID, "sandbox-review-id", "", "Sandbox UUID")
}

func runSandboxUnknownReview() error {
	if sandboxReviewDSNFile == "" || sandboxReviewKubeconfig == "" {
		return fmt.Errorf("database DSN file and kubeconfig are required")
	}
	raw, err := readImagePrivateFile(sandboxReviewDSNFile)
	if err != nil {
		return fmt.Errorf("read Sandbox DSN file: %w", err)
	}
	defer clear(raw)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, err := data.OpenPostgres(ctx, strings.TrimSpace(string(raw)))
	if err != nil {
		return err
	}
	defer repo.Close()
	cfg, err := clientcmd.BuildConfigFromFlags("", sandboxReviewKubeconfig)
	if err != nil {
		return err
	}
	cfg.Timeout = 5 * time.Second
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	provider, err := data.NewKubernetesProvider(client, sandboxReviewCluster)
	if err != nil {
		return err
	}
	report, err := repo.ReviewUnknownCreate(ctx, provider, sandboxReviewTenant, sandboxReviewID)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
