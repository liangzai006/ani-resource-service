//go:build instanceintegration

package instance_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
)

// The worker, admissions, cleanup fact and retry event all use production
// business code and PostgreSQL. Only Kubernetes and Gov are external doubles.
func TestInstanceOwnerCleanupOutboxDispatchWithoutACC(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := create()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	p := &provider{failPVC: true}
	worker, err := biz.NewWorker(f.repo, p, "owner-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	var i biz.Instance
	for n := 0; n < 3; n++ {
		i = step(t, f, worker, c)
	}
	command(t, f, c, i, "delete")
	for n := 0; n < 10; n++ {
		i = step(t, f, worker, c)
	}
	remote := &releaseEndpoint{}
	reporter, err := data.NewQuota(f.repo, remote)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || dispatched || remote.calls != 0 {
		t.Fatal("unfinished PVC cleanup was delivered to Gov", dispatched, err, remote.calls)
	}
	var events int
	if err := f.runtime.QueryRow(ctx, `SELECT count(*) FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID).Scan(&events); err != nil || events != 0 || !i.CleanupCompletedAt.IsZero() || i.QuotaReturned || !i.ComputeCleaned {
		t.Fatal("cleanup event preceded the owner cleanup fact", events, i, err)
	}
	p.failPVC = false
	for n := 0; n < 6; n++ {
		i = step(t, f, worker, c)
	}
	if i.Observed != biz.Deleted || i.CleanupCompletedAt.IsZero() || !i.WorkspaceCleaned || i.QuotaReturned || remote.calls != 0 {
		t.Fatal("owner cleanup depended on Gov or ACC", i, remote.calls)
	}
	var event, original string
	if err := f.runtime.QueryRow(ctx, `SELECT event_id::text,original_create_operation_id::text FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID).Scan(&event, &original); err != nil || original != c.OperationID {
		t.Fatal("cleanup did not atomically persist original release event", original, err)
	}
	if dispatched, err := reporter.Dispatch(ctx); err == nil || !dispatched || remote.calls != 1 || remote.applied != 1 {
		t.Fatal("lost Gov ACK was accepted", dispatched, err, remote.calls)
	}
	i, err = f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || i.QuotaReturned {
		t.Fatal("lost ACK settled the local ledger", i, err)
	}
	// A new delivery adapter has no Kubernetes provider and must recover only
	// the persisted event. The retry's request must match the original event.
	reporter, err = data.NewQuota(f.repo, remote)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || dispatched || remote.calls != 1 {
		t.Fatal("failed event ignored its bounded retry delay", dispatched, err, remote.calls)
	}
	var retryDelayed bool
	if err := f.runtime.QueryRow(ctx, `SELECT next_attempt_at>clock_timestamp() AND next_attempt_at<=clock_timestamp()+interval '5 seconds' FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID).Scan(&retryDelayed); err != nil || !retryDelayed {
		t.Fatal("failed event has no bounded persistent retry", retryDelayed, err)
	}
	forceReleaseDue(t, f, c)
	if dispatched, err := reporter.Dispatch(ctx); err != nil || !dispatched {
		t.Fatal("recreated adapter did not retry the persisted event", dispatched, err)
	}
	i, err = f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || !i.QuotaReturned || remote.calls != 2 || remote.applied != 1 || remote.request.ReleaseEventId != event || remote.request.CreateOperationId != c.OperationID {
		t.Fatal("ALREADY_APPLIED did not settle the same original event", i, err, remote.calls)
	}
	op, err := f.repo.GetOperation(ctx, c.TenantID, i.LastOperationID)
	if err != nil || op.State != "completed" {
		t.Fatal("release ACK did not durably complete DELETE", op, err)
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || dispatched || remote.calls != 2 {
		t.Fatal("settled event was retransmitted", dispatched, err, remote.calls)
	}
}

// Only the retry clock moves. Business cleanup and event creation remain the
// production worker/repository's responsibility.
func forceReleaseDue(t *testing.T, f *fixture, c biz.Create) {
	t.Helper()
	if _, err := f.runtime.Exec(context.Background(), `UPDATE instance.instance_quota_release_outbox SET next_attempt_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID); err != nil {
		t.Fatal(err)
	}
}

func TestInstanceLegacyReleaseOutboxNeedsOwnerCleanupFact(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		name := "unacknowledged"
		if acknowledged {
			name = "acknowledged"
		}
		t.Run(name, func(t *testing.T) {
			f := database(t)
			ctx := context.Background()
			c := create()
			if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
				t.Fatal(err)
			}
			p := &provider{failPVC: true}
			worker, err := biz.NewWorker(f.repo, p, "legacy-owner")
			if err != nil {
				t.Fatal(err)
			}
			var i biz.Instance
			for n := 0; n < 3; n++ {
				i = step(t, f, worker, c)
			}
			command(t, f, c, i, "delete")
			for n := 0; n < 8; n++ {
				i = step(t, f, worker, c)
			}
			// Reproduce the pre-upgrade row, which had a stable event but no
			// owner cleanup columns. Do not synthesize a new cleanup fact.
			event := uuid.NewSHA1(uuid.NameSpaceOID, []byte("resource-release:"+c.TenantID+":"+c.ID+":"+c.OperationID)).String()
			if _, err := f.runtime.Exec(ctx, `INSERT INTO instance.instance_quota_release_outbox(tenant_id,instance_id,event_id,original_create_operation_id,reason,acknowledged) VALUES($1,$2,$3,$4,'RESOURCE_RELEASED',$5)`, c.TenantID, c.ID, event, c.OperationID, acknowledged); err != nil {
				t.Fatal(err)
			}
			remote := &releaseEndpoint{immediateReceipt: true}
			reporter, err := data.NewQuota(f.repo, remote)
			if err != nil {
				t.Fatal(err)
			}
			if dispatched, err := reporter.Dispatch(ctx); err != nil || dispatched || remote.calls != 0 {
				t.Fatal("legacy event delivered before owner cleanup", dispatched, err, remote.calls)
			}
			if err := reporter.Report(ctx, i); err == nil || remote.calls != 0 {
				t.Fatal("legacy event bypassed the missing owner cleanup fact", err, remote.calls)
			}
			p.failPVC = false
			for n := 0; n < 6; n++ {
				i = step(t, f, worker, c)
			}
			if i.Observed != biz.Deleted || i.CleanupCompletedAt.IsZero() {
				t.Fatal("legacy DELETE did not recover through real owner cleanup", i)
			}
			var storedEvent string
			var hasFact bool
			if err := f.runtime.QueryRow(ctx, `SELECT event_id::text,cleanup_completed_at IS NOT NULL AND cleanup_version IS NOT NULL FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID).Scan(&storedEvent, &hasFact); err != nil || storedEvent != event || !hasFact {
				t.Fatal("legacy recovery replaced its event or lost the cleanup fact", storedEvent, hasFact, err)
			}
			dispatched, err := reporter.Dispatch(ctx)
			if err != nil || dispatched == acknowledged {
				t.Fatal("legacy ACK state was not respected", dispatched, acknowledged, err)
			}
			wantCalls := 1
			if acknowledged {
				wantCalls = 0
			}
			i, err = f.repo.Get(ctx, c.TenantID, c.ID)
			if err != nil || !i.QuotaReturned || remote.calls != wantCalls {
				t.Fatal("legacy event refunded twice or failed to settle", i, err, remote.calls)
			}
		})
	}
}

func TestInstanceLegacyCleanupFlagsCannotSealAfterFailedObservation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		observation biz.Observation
		observeErr  error
	}{
		{name: "provider unavailable", observeErr: errors.New("external Kubernetes unavailable")},
		{name: "residual volume", observation: biz.Observation{VolumeExists: true, VolumeName: "original-volume", VolumeUID: "original-volume-uid"}},
		{name: "workspace observation failed", observation: biz.Observation{WorkspaceReason: "WORKSPACE_OBSERVATION_FAILED"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := database(t)
			ctx := context.Background()
			c := create()
			c.Spec.Network = &biz.NetworkSelection{VPCID: "vpc_" + strings.Repeat("a", 32), SubnetID: "subnet_" + strings.Repeat("b", 32)}
			if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
				t.Fatal(err)
			}
			i, err := f.repo.Get(ctx, c.TenantID, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			command(t, f, c, i, "delete")
			i, err = f.repo.Get(ctx, c.TenantID, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			// Only this pre-upgrade fixture is injected: its historical booleans
			// cannot establish this version's new owner cleanup fact.
			i.ComputeCleaned, i.WorkspaceCleaned, i.VolumeReferencesReleased = true, true, true
			i.Network.Cleaned, i.Network.ReleaseAcknowledged = true, true
			i.ExternalComputeStarted = true
			raw, err := json.Marshal(i)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.runtime.Exec(ctx, `UPDATE instance.instances SET snapshot=$3 WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID, raw); err != nil {
				t.Fatal(err)
			}
			p := &provider{obs: tc.observation, observeErr: tc.observeErr}
			worker, err := biz.NewWorker(f.repo, p, "legacy-observation")
			if err != nil {
				t.Fatal(err)
			}
			for n := 0; n < 3; n++ {
				i = step(t, f, worker, c)
			}
			if !i.CleanupCompletedAt.IsZero() || i.QuotaReturned {
				t.Fatal("historical flags bypassed this turn's failed cleanup", i)
			}
			var events int
			if err := f.runtime.QueryRow(ctx, `SELECT count(*) FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID).Scan(&events); err != nil || events != 0 {
				t.Fatal("failed observation sealed a refund event from historical flags", events, err)
			}
			remote := &releaseEndpoint{immediateReceipt: true}
			reporter, err := data.NewQuota(f.repo, remote)
			if err != nil {
				t.Fatal(err)
			}
			if dispatched, err := reporter.Dispatch(ctx); err != nil || dispatched || remote.calls != 0 {
				t.Fatal("failed cleanup became eligible for Gov delivery", dispatched, err, remote.calls)
			}
		})
	}
}
