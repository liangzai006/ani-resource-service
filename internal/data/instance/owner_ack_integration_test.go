//go:build instanceintegration

package instance_test

import (
	"context"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
)

func TestInstanceWorkerFinishPreservesCommittedReleaseACK(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := create()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	w, err := biz.NewWorker(f.repo, &provider{}, "cleanup-before-ack")
	if err != nil {
		t.Fatal(err)
	}
	i := step(t, f, w, c)
	command(t, f, c, i, "delete")
	for n := 0; n < 6; n++ {
		i = step(t, f, w, c)
	}
	if i.CleanupCompletedAt.IsZero() || i.QuotaReturned {
		t.Fatal("expected complete cleanup awaiting its original event", i)
	}
	if _, err := f.runtime.Exec(ctx, "UPDATE instance.instance_work SET due_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2", c.TenantID, c.ID); err != nil {
		t.Fatal(err)
	}
	work, err := f.repo.Claim(ctx, "worker-overlapping-ack", time.Minute)
	if err != nil || work == nil {
		t.Fatal("claim", err)
	}
	remote := &releaseEndpoint{immediateReceipt: true}
	reporter, err := data.NewQuota(f.repo, remote)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || !dispatched {
		t.Fatal("dispatch", dispatched, err)
	}
	// This worker read the pending state before the independent dispatcher
	// committed its ACK. Finishing that old work must retain the settled event.
	if err := f.repo.Finish(ctx, *work, biz.Progress{Instance: work.Instance, CleanupComplete: true}); err != nil {
		t.Fatal(err)
	}
	i, err = f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || !i.QuotaReturned || i.Reason != "" || remote.calls != 1 {
		t.Fatal("worker overwrote a committed quota ACK", i, err, remote.calls)
	}
	op, err := f.repo.GetOperation(ctx, c.TenantID, i.LastOperationID)
	if err != nil || op.State != "completed" {
		t.Fatal("worker reopened the acknowledged DELETE", op, err)
	}
}
