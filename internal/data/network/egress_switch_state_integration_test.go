package data_test

import (
	"errors"
	"strings"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"k8s.io/client-go/rest"
)

func TestEgressDependencyUnavailableIsNotOwnershipConflict(t *testing.T) {
	f, _ := switchEgress(t)
	sw, err := f.e.CreateVirtualSwitch(f.ctx, biz.VirtualSwitchIntent{Name: "dependency-state", IdempotencyKey: "switch"})
	if err != nil {
		t.Fatal(err)
	}
	vlan, err := f.e.CreatePlatform(f.ctx, biz.PlatformIntent{Kind: "create_vlan", Name: "vlan", IdempotencyKey: "vlan", Vlan: &biz.VlanConfig{VlanID: 0, ClusterID: "test-cluster", VirtualSwitchID: sw.ID, DeviceName: "ens35"}})
	if err != nil {
		t.Fatal(err)
	}
	// The physical boundary is controlled; the dependency, pool and provider
	// bindings remain the real persisted product objects.
	if _, err = f.owner.Exec(f.ctx, "UPDATE network_virtual_switches SET state='available',binding_state='configured' WHERE resource_id=$1", sw.ID); err != nil {
		t.Fatal(err)
	}
	f.drive(t, func() bool {
		v, e := f.e.GetPlatform(f.ctx, "vlan", vlan.ID)
		return e == nil && v.State == biz.Available
	})
	gateway := f.platform(t, biz.PlatformIntent{Kind: "create_egress_gateway", Name: "gateway", IdempotencyKey: "gateway"})
	pool := f.platform(t, biz.PlatformIntent{Kind: "create_public_pool", Name: "pool", IdempotencyKey: "pool", Pool: &biz.PublicPoolConfig{Mode: "underlay", GatewayID: gateway.ID, CIDR: "192.0.2.0/24", OVNGatewayIP: "192.0.2.1", UpstreamGatewayIP: "192.0.2.254", VlanNetworkID: vlan.ID}})
	provider, err := data.NewKCProvider(f.p, &rest.Config{Host: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.owner.Exec(f.ctx, "UPDATE network_virtual_switches SET state='degraded',binding_state='degraded' WHERE resource_id=$1", sw.ID); err != nil {
		t.Fatal(err)
	}
	for _, value := range []biz.PlatformResource{vlan, pool} {
		t.Run(value.Kind, func(t *testing.T) {
			target := biz.ProviderTarget{Kind: value.Kind, ResourceID: value.ID}
			if err := f.owner.QueryRow(f.ctx, "SELECT binding_id,provider_uid FROM network_platform_resources WHERE resource_id=$1", value.ID).Scan(&target.BindingID, &target.KnownIdentity); err != nil {
				t.Fatal(err)
			}
			_, err := provider.Observe(f.ctx, target)
			var failure *biz.ProviderError
			if !errors.As(err, &failure) || failure.Kind != biz.ProviderTemporary || failure.Cause == nil || !strings.Contains(failure.Cause.Error(), "virtual switch") {
				t.Fatalf("unavailable dependency must not claim ownership conflict: %#v", err)
			}
		})
	}
	if _, err = f.owner.Exec(f.ctx, "UPDATE network_device_adoptions SET retired=true WHERE virtual_switch_id=$1", sw.ID); err != nil {
		t.Fatal(err)
	}
	target := biz.ProviderTarget{Kind: pool.Kind, ResourceID: pool.ID}
	if err = f.owner.QueryRow(f.ctx, "SELECT binding_id,provider_uid FROM network_platform_resources WHERE resource_id=$1", pool.ID).Scan(&target.BindingID, &target.KnownIdentity); err != nil {
		t.Fatal(err)
	}
	_, err = provider.Observe(f.ctx, target)
	var conflict *biz.ProviderError
	if !errors.As(err, &conflict) || conflict.Kind != biz.ProviderConflict {
		t.Fatalf("retired device must still conflict: %#v", err)
	}
}
