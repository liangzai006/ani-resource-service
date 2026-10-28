package data_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func managedPoolIntent(key string) biz.PlatformIntent {
	return biz.PlatformIntent{Kind: "create_public_pool", Name: key, IdempotencyKey: key, Pool: &biz.PublicPoolConfig{Mode: "overlay", CIDR: "198.51.100.0/24", OVNGatewayIP: "198.51.100.1"}}
}

type heldGatewayProvider struct {
	*egressProvider
	hold   bool
	reject bool
}

func (p *heldGatewayProvider) Observe(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	v, err := p.egressProvider.Observe(ctx, t)
	if t.Kind == "egress_gateway" && p.hold {
		v.Ready = false
	}
	return v, err
}
func (p *heldGatewayProvider) EnsureEgress(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	if t.Kind == "egress_gateway" && p.reject {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderReject}
	}
	v, err := p.egressProvider.EnsureEgress(ctx, t)
	if t.Kind == "egress_gateway" && p.hold {
		v.Ready = false
	}
	return v, err
}
func restartManagedWorker(t *testing.T, f *egressFixture, provider biz.ResourceProvider) {
	t.Helper()
	policy := biz.DefaultWorkerPolicy()
	policy.ObserveEvery = 100 * time.Millisecond
	policy.RetryMin = time.Millisecond
	policy.RetryMax = 10 * time.Millisecond
	var err error
	f.w, err = biz.NewWorker(f.p, provider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
}
func TestManagedPoolGatewayAtomicReplayReadyAndCleanup(t *testing.T) {
	f := newEgressFixture(t)
	provider := &heldGatewayProvider{egressProvider: f.provider, hold: true}
	restartManagedWorker(t, f, provider)
	input := managedPoolIntent("owned-pool")
	accepted, err := f.e.CreatePlatform(f.ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	gatewayID := accepted.Pool.GatewayID
	if gatewayID == "" || input.Pool.GatewayID != "" {
		t.Fatal("gateway not generated, or caller intent mutated")
	}
	for i := 0; i < 30; i++ {
		if _, err = f.w.Step(f.ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	f.provider.mu.Lock()
	_, premature := f.provider.objects[accepted.ID]
	f.provider.mu.Unlock()
	if premature {
		t.Fatal("pool created before owned gateway readiness")
	}
	restartManagedWorker(t, f, provider)
	replay, err := f.e.CreatePlatform(f.ctx, input)
	if err != nil || !samePlatformJSON(accepted, replay) {
		t.Fatalf("replay changed: %v %+v", err, replay)
	}
	different := input
	different.Name = "changed"
	if _, err = f.e.CreatePlatform(f.ctx, different); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatalf("changed intent replay: %v", err)
	}
	if _, err = f.e.CreatePlatform(context.Background(), managedPoolIntent("unauthorized")); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatalf("authorization bypass: %v", err)
	}
	provider.hold = false
	f.drive(t, func() bool {
		v, e := f.e.GetPlatform(f.ctx, "public_pool", accepted.ID)
		return e == nil && v.State == biz.Available
	})
	var owned int
	if err = f.owner.QueryRow(f.ctx, "SELECT count(*) FROM network_public_pools WHERE resource_id=$1 AND gateway_id=$2 AND managed_gateway", accepted.ID, gatewayID).Scan(&owned); err != nil || owned != 1 {
		t.Fatal("ownership not durable", err, owned)
	}
	// A second pool must never acquire this internally owned gateway.
	borrowed := managedPoolIntent("borrowed")
	borrowed.Pool.GatewayID = gatewayID
	borrowed.Pool.CIDR = "203.0.113.0/24"
	borrowed.Pool.OVNGatewayIP = "203.0.113.1"
	if _, err = f.e.CreatePlatform(f.ctx, borrowed); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatalf("owned gateway was shared: %v", err)
	}
	if _, err = f.e.DeletePlatform(f.ctx, "egress_gateway", gatewayID); biz.ReasonOf(err) != biz.ResourceInUse {
		t.Fatalf("owned gateway directly removed: %v", err)
	}
	if _, err = f.e.DeletePlatform(f.ctx, "public_pool", accepted.ID); err != nil {
		t.Fatal(err)
	}
	restartManagedWorker(t, f, provider)
	f.drive(t, func() bool {
		v, e := f.e.GetPlatform(f.ctx, "public_pool", accepted.ID)
		return e == nil && v.State == biz.Deleted
	})
	gateway, err := f.e.GetPlatform(f.ctx, "egress_gateway", gatewayID)
	if err != nil || gateway.State != biz.Deleted {
		t.Fatal("pool completed before gateway cleanup", gateway, err)
	}
	// The pre-existing explicit gateway is retained.
	explicit, err := f.e.GetPlatform(f.ctx, "egress_gateway", f.pool.Pool.GatewayID)
	if err != nil || explicit.State != biz.Available {
		t.Fatal("unrelated gateway changed", explicit, err)
	}
	replay, err = f.e.CreatePlatform(f.ctx, input)
	if err != nil || !samePlatformJSON(replay, accepted) {
		t.Fatal("deleted owner replay resurrected child", err)
	}
}

func TestManagedPoolGatewayCancelPendingAndRejectedCreate(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "rejected"}[reject], func(t *testing.T) {
			f := newEgressFixture(t)
			provider := &heldGatewayProvider{egressProvider: f.provider, hold: true, reject: reject}
			restartManagedWorker(t, f, provider)
			pool, err := f.e.CreatePlatform(f.ctx, managedPoolIntent("cancel-pool"))
			if err != nil {
				t.Fatal(err)
			}
			if reject {
				f.drive(t, func() bool {
					v, e := f.e.GetPlatform(f.ctx, "public_pool", pool.ID)
					return e == nil && v.State == biz.Failed
				})
			}
			if _, err = f.e.DeletePlatform(f.ctx, "public_pool", pool.ID); err != nil {
				t.Fatal(err)
			}
			restartManagedWorker(t, f, provider)
			f.drive(t, func() bool {
				v, e := f.e.GetPlatform(f.ctx, "public_pool", pool.ID)
				return e == nil && v.State == biz.Deleted
			})
			gateway, err := f.e.GetPlatform(f.ctx, "egress_gateway", pool.Pool.GatewayID)
			if err != nil || gateway.State != biz.Deleted {
				t.Fatal("orphaned owned gateway", gateway, err)
			}
		})
	}
}

func samePlatformJSON(a, b biz.PlatformResource) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
