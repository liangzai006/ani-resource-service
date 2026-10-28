package data_test

import (
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"testing"
)

func TestPublicPoolAllocationWithoutManualVerification(t *testing.T) {
	f := newEgressFixtureWithPublicVerification(t, false)
	pool, err := f.e.GetPlatform(f.ctx, "public_pool", f.pool.ID)
	if err != nil || pool.Verification != nil || !pool.AllocationEnabled || !pool.IsDefault {
		t.Fatalf("unverified public pool did not open: %v %v", pool, err)
	}
	caps, err := f.e.GetPlatformCapabilities(f.ctx)
	if err != nil || !caps.PublicAddress.Ready {
		t.Fatalf("unverified default public pool unavailable: %v %v", caps, err)
	}
	eip := f.eip(t, "no-manual-egress-evidence")
	if eip.State != biz.Available || eip.Address == "" {
		t.Fatalf("EIP allocation failed: %v", eip)
	}
	f.setPool(t, biz.PlatformIntent{Kind: "set_pool_allocation", ID: pool.ID, Enabled: false, IdempotencyKey: "close-without-evidence"})
	if _, err = f.e.CreateEIP(f.ctx, biz.EgressIntent{Name: "closed", IdempotencyKey: "closed-no-evidence"}); err == nil {
		t.Fatal("closed pool accepted a new EIP")
	}
	intranet := f.platform(t, intranetIntent("still-requires-intranet-verification", "10.232.254.0/24", "10.232.254.1"))
	if _, err = f.e.SetPool(f.ctx, biz.PlatformIntent{Kind: "set_intranet_pool_allocation", ID: intranet.ID, ExpectedVersion: intranet.Version, Enabled: true, IdempotencyKey: "intranet-not-authorized-by-public-change"}); err == nil {
		t.Fatal("public policy change removed intranet verification")
	}
	_, err = f.owner.Exec(f.ctx, "UPDATE network_public_pools SET allocation_enabled=true WHERE resource_id=$1", intranet.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "network_pool_allocation_verification" {
		t.Fatalf("intranet database constraint lost: %v", err)
	}

}
