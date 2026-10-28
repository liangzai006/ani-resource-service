package main

import (
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
)

// Both runtime modes use the same request budgets and observation lifecycle.
func openNetworkProvider(repository *data.Postgres, network *conf.Network) (*data.KCProvider, *data.KCObservation, error) {
	clientPolicy := data.KCClientPolicy{QPS: 5, Burst: 10}
	if o := network.Observation; o != nil {
		clientPolicy = data.KCClientPolicy{QPS: float32(o.RequestQps), Burst: int(o.RequestBurst)}
	}
	provider, err := data.OpenKCProvider(repository, network.Kubeconfig, clientPolicy)
	if err != nil {
		return nil, nil, err
	}
	options := data.DefaultObservationOptions()
	if lb := network.LoadBalancer; lb != nil {
		if err = provider.ConfigureLoadBalancer(data.LoadBalancerInstallation{Fingerprint: lb.InstallationFingerprint, ControllerImageID: lb.ControllerImageId, EnvoyImageID: lb.EnvoyImageId, ShutdownImageID: lb.ShutdownImageId, KCImageID: lb.KcImageId}); err != nil {
			return nil, nil, err
		}
	}
	if o := network.Observation; o != nil {
		options = data.ObservationOptions{AuditInterval: o.AuditInterval.AsDuration(), AuditJitter: o.AuditJitter.AsDuration(), AuditTimeout: o.AuditTimeout.AsDuration(), FlushInterval: o.FlushInterval.AsDuration(), QueueCapacity: int(o.QueueCapacity)}
	}
	observation, err := provider.EnableObservation(options)
	if err != nil {
		return nil, nil, err
	}
	return provider, observation, nil
}
