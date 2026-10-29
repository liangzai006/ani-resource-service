package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	networkdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/vm"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

type vmSettings struct {
	DatabaseDSNFile, ConsoleAddress, GuestCredentialKeyFile string
	NetworkServerSocket                                     string
	Service                                                 service.Config
	Networks                                                []networkdata.ManagedVMNAD
}
type vmComponents struct {
	repo    *data.Postgres
	service *service.Service
	worker  *biz.Worker
	console *server.InstanceTerminalServer
}

func (c *vmComponents) Close() {
	if c != nil && c.repo != nil {
		c.repo.Close()
	}
}
func runVMMigration() error {
	raw, e := readImagePrivateFile(os.Getenv("ANI_VM_OWNER_DSN_FILE"))
	if e != nil {
		return e
	}
	defer clear(raw)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return data.Migrate(ctx, strings.TrimSpace(string(raw)), os.Getenv("ANI_VM_RUNTIME_ROLE"))
}
func wireVM(ctx context.Context, c *conf.Network, compute *computeComponents, repository *networkdata.Postgres, provider *networkdata.KCProvider, attachments *networkbiz.Attachments, instances *instanceComponents, legacy networkbiz.InstanceConsumer, freshness time.Duration) (*vmComponents, networkbiz.InstanceConsumer, error) {
	path := os.Getenv("ANI_VM_CONFIG_FILE")
	if path == "" {
		return nil, networkbiz.NewVMConsumerRouter(legacy, nil), nil
	}
	raw, e := readImagePrivateFile(path)
	if e != nil {
		return nil, nil, e
	}
	defer clear(raw)
	var cfg vmSettings
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(&cfg); e != nil || d.Decode(new(any)) != io.EOF || compute == nil || !filepath.IsAbs(cfg.DatabaseDSNFile) || filepath.Clean(cfg.DatabaseDSNFile) != filepath.Clean(compute.settings.DatabaseDSNFile) || cfg.Service.ClusterID != c.ClusterId {
		return nil, nil, fmt.Errorf("VM requires managed compute database and cluster configuration")
	}
	if e = provider.ConfigureVMNetworks(cfg.Networks); e != nil {
		return nil, nil, e
	}
	if e = provider.ConfigureVMAutoNetwork(networkdata.VMAutoNetworkConfig{ServiceAccount: cfg.Service.ServiceAccount, ServerSocket: cfg.NetworkServerSocket}); e != nil {
		return nil, nil, e
	}
	if e = repository.SetVMNetworkResolver(provider, freshness); e != nil {
		return nil, nil, e
	}
	dsn, e := readImagePrivateFile(cfg.DatabaseDSNFile)
	if e != nil {
		return nil, nil, e
	}
	defer clear(dsn)
	repo, e := data.OpenPostgres(ctx, strings.TrimSpace(string(dsn)))
	if e != nil {
		return nil, nil, e
	}
	valid := false
	defer func() {
		if !valid {
			repo.Close()
		}
	}()
	if cfg.GuestCredentialKeyFile != "" {
		key, e := readImagePrivateFile(cfg.GuestCredentialKeyFile)
		if e != nil {
			return nil, nil, e
		}
		defer clear(key)
		if e = repo.ConfigureGuestCredentialKey(key); e != nil {
			return nil, nil, e
		}
	}
	clientConfig, e := clientcmd.BuildConfigFromFlags("", c.Kubeconfig)
	if e != nil {
		return nil, nil, e
	}
	clientConfig.Timeout = 10 * time.Second
	client, e := dynamic.NewForConfig(clientConfig)
	if e != nil {
		return nil, nil, e
	}
	native, e := data.NewNative(client, clientConfig)
	if e != nil {
		return nil, nil, e
	}
	native.SetGuestCredentialReader(repo.GuestPasswordHash)
	if e = native.ConfigureLogs(clientConfig, cfg.Service.LogLimits); e != nil {
		return nil, nil, e
	}
	networkPort, e := data.NewNetwork(attachments, repo, native)
	if e != nil {
		return nil, nil, e
	}
	environment, e := data.NewEnvironment(native, c.NamespacePrefix, func(ctx context.Context, tenant, cluster, namespace string, s biz.NetworkSelection) (biz.NetworkSelection, error) {
		policy, e := provider.LookupManagedVMNetwork(ctx, tenant, s.VPCID, s.SubnetID, cluster, namespace)
		if e != nil {
			return biz.NetworkSelection{}, e
		}
		if s.NADName != "" && (s.NADName != policy.NADName || s.NADUID != policy.NADUID || s.NADDigest != policy.NADConfigDigest) {
			return biz.NetworkSelection{}, biz.ErrChanged
		}
		plan, e := repository.ResolveVMNetworkSelection(ctx, networkbiz.PrepareAttachment{TenantID: tenant, VPCID: s.VPCID, SubnetID: s.SubnetID, ClusterID: cluster, Namespace: namespace, WorkloadMode: "vm_managed", VMExpectation: &networkbiz.VMNetworkExpectation{NADName: policy.NADName, NADUID: policy.NADUID, NADConfigDigest: policy.NADConfigDigest}}, freshness)
		if e != nil {
			return biz.NetworkSelection{}, e
		}
		return biz.NetworkSelection{VPCID: s.VPCID, SubnetID: s.SubnetID, NADName: plan.NADName, NADUID: plan.NADUID, NADDigest: plan.NADConfigDigest}, nil
	})
	if e != nil {
		return nil, nil, e
	}
	handler, e := service.NewService(repo, compute.service, environment, native, cfg.Service)
	if e != nil {
		return nil, nil, e
	}
	handler.SetAddressReader(networkPort)
	if instances != nil && instances.service != nil {
		instances.service.SetVMMetricScope(func(ctx context.Context, tenant, id string) (string, string, bool, error) {
			v, e := repo.Get(ctx, tenant, id)
			if errors.Is(e, biz.ErrNotFound) {
				return "", "", false, nil
			}
			if e != nil {
				return "", "", false, e
			}
			return v.Spec.ClusterID, v.Spec.Namespace, true, nil
		})
	}
	worker, e := biz.NewWorker(repo, native, networkPort)
	if e != nil {
		return nil, nil, e
	}
	component := &vmComponents{repo: repo, service: handler, worker: worker}
	if instances != nil {
		instances.terminal.RegisterVMConsole(handler)
	} else {
		component.console, e = server.NewInstanceTerminalServer(cfg.ConsoleAddress, os.Getenv("ANI_NETWORK_CLIENT_CA"), os.Getenv("ANI_NETWORK_TLS_CERT"), os.Getenv("ANI_NETWORK_TLS_KEY"), nil, handler)
		if e != nil {
			return nil, nil, e
		}
	}
	valid = true
	return component, networkbiz.NewVMConsumerRouter(legacy, networkPort), nil
}
