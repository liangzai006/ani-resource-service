package data_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func TestPlatformEIPInventoryAndOwnerRelease(t *testing.T) {
	f := newEgressFixture(t)
	a := f.eip(t, "platform-alpha")
	admin := f.ctx
	other := uuid.NewString()
	f.ctx = biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: other, Attribution: biz.Attribution{Actor: "tenant-b", DirectCaller: "fixture"}})
	b := f.eip(t, "platform-beta")
	f.ctx = admin
	for _, ctx := range []context.Context{context.Background(), biz.WithEgressCaller(context.Background(), biz.EgressCaller{TenantID: f.tenant, DelegatedTenants: []string{other}, Attribution: biz.Attribution{Actor: "tenant", DirectCaller: "fixture"}})} {
		if _, _, err := f.e.ListPlatformEIPs(ctx, biz.PlatformEIPFilter{PageWindow: biz.PageWindow{Limit: 20}}); biz.ReasonOf(err) != biz.PermissionDenied {
			t.Fatalf("tenant reached platform inventory: %v", err)
		}
		if _, err := f.e.GetPlatformEIP(ctx, b.ID); biz.ReasonOf(err) != biz.PermissionDenied {
			t.Fatalf("tenant reached platform detail: %v", err)
		}
		if _, err := f.e.ReleasePlatformEIP(ctx, b.ID); biz.ReasonOf(err) != biz.PermissionDenied {
			t.Fatalf("tenant reached platform release: %v", err)
		}
	}
	rows, total, err := f.e.ListPlatformEIPs(admin, biz.PlatformEIPFilter{PageWindow: biz.PageWindow{Limit: 1, Offset: 1}})
	if err != nil || total != 2 || len(rows) != 1 || rows[0].ID != a.ID {
		t.Fatalf("direct second page: rows=%+v total=%d err=%v", rows, total, err)
	}
	for _, tc := range []struct {
		filter biz.PlatformEIPFilter
		want   int64
	}{
		{biz.PlatformEIPFilter{TenantID: other}, 1},
		{biz.PlatformEIPFilter{Name: "beta"}, 1},
		{biz.PlatformEIPFilter{Address: b.Address}, 1},
		{biz.PlatformEIPFilter{PoolID: f.pool.ID}, 2},
		{biz.PlatformEIPFilter{State: "failed"}, 0},
		{biz.PlatformEIPFilter{Scope: "intranet"}, 0},
	} {
		tc.filter.PageWindow = biz.PageWindow{Limit: 20}
		got, count, e := f.e.ListPlatformEIPs(admin, tc.filter)
		if e != nil || count != tc.want || int64(len(got)) != tc.want {
			t.Fatalf("filter %+v: len=%d total=%d err=%v", tc.filter, len(got), count, e)
		}
	}
	rows, total, err = f.e.ListPlatformEIPs(admin, biz.PlatformEIPFilter{PageWindow: biz.PageWindow{Limit: 20, Offset: 1000}})
	if err != nil || len(rows) != 0 || total != 2 {
		t.Fatalf("empty page lost total: %+v %d %v", rows, total, err)
	}
	detail, err := f.e.GetPlatformEIP(admin, b.ID)
	if err != nil || detail.TenantID != other || detail.PoolID != f.pool.ID || detail.PoolName != "fixture-pool" {
		t.Fatal(detail, err)
	}
	released, err := f.e.ReleasePlatformEIP(admin, b.ID)
	if err != nil || released.State != biz.Deleting || released.TenantID != other {
		t.Fatal(released, err)
	}
	unchanged, err := f.e.GetPlatformEIP(admin, a.ID)
	if err != nil || unchanged.State != biz.Available {
		t.Fatal("release touched another tenant", unchanged, err)
	}
}

func TestPlatformEIPReleaseRetainsClaimAndSystemProtection(t *testing.T) {
	f := newEgressFixture(t)
	vpc := availableEgressVPC(t, f, "platform-release")
	eip := f.eip(t, "claimed")
	binding, err := f.e.BindVPCSnat(f.ctx, biz.EgressIntent{VPCID: vpc.ID, EIPID: eip.ID, IdempotencyKey: "claim"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.ReleasePlatformEIP(f.ctx, eip.ID); biz.ReasonOf(err) != biz.EIPInUse {
		t.Fatalf("platform release bypassed active claim: %v", err)
	}
	got, err := f.e.GetPlatformEIP(f.ctx, eip.ID)
	if err != nil || got.BindingTarget == nil || got.BindingTarget.ID != binding.ID {
		t.Fatal(got, err)
	}
	var systemID string
	if err = f.owner.QueryRow(f.ctx, "SELECT eip_id FROM network_eips WHERE tenant_id=$1 AND system_owner_vpc=$2", f.tenant, vpc.ID).Scan(&systemID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.GetPlatformEIP(f.ctx, systemID); err != nil {
		t.Fatalf("platform cannot inspect system address: %v", err)
	}
	if _, err = f.e.ReleasePlatformEIP(f.ctx, systemID); biz.ReasonOf(err) != biz.PermissionDenied {
		t.Fatalf("system owner protection bypassed: %v", err)
	}
}
