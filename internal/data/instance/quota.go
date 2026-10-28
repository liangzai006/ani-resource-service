package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	quota "github.com/zhangzhe-ctrl/ani-governance/api/quota/gen/go/quota/service/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"google.golang.org/grpc"
)

type QuotaClient interface {
	ReportResourceReleased(context.Context, *quota.ReportResourceReleasedRequest, ...grpc.CallOption) (*quota.ReportResourceReleasedResponse, error)
}
type Quota struct {
	repo   *Postgres
	client QuotaClient
}

func NewQuota(repo *Postgres, client QuotaClient) (*Quota, error) {
	if repo == nil || client == nil {
		return nil, biz.ErrInvalid
	}
	return &Quota{repo, client}, nil
}

func releaseEventID(i biz.Instance) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("resource-release:"+i.TenantID+":"+i.ID+":"+i.CreateOperationID)).String()
}

// Finish holds the current intent and worker lease locks. The final owner
// cleanup fact and its stable event become eligible in the same transaction.
func sealQuotaRelease(ctx context.Context, tx pgx.Tx, i *biz.Instance) error {
	if !biz.OwnerCleanupReady(*i) {
		return nil
	}
	var action string
	if err := tx.QueryRow(ctx, "SELECT action FROM instance.instance_operations WHERE tenant_id=$1 AND instance_id=$2 AND operation_id=$3", i.TenantID, i.ID, i.LastOperationID).Scan(&action); err != nil {
		return err
	}
	if action != "delete" || i.LastOperationID == i.CreateOperationID {
		return biz.ErrInvalid
	}
	if i.CleanupCompletedAt.IsZero() {
		if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&i.CleanupCompletedAt); err != nil {
			return err
		}
	}
	i.Observed = biz.Deleted
	if i.Spec.ComputeType == biz.TypeGPU && !i.QuotaReturned {
		i.Reason = "QUOTA_RETURN_PENDING"
	}
	if i.Spec.ComputeType == biz.TypeCPU {
		i.QuotaReturned = true
		i.Reason = ""
		return nil
	}
	if i.QuotaReturned {
		return nil
	}
	var event, create, reason string
	var acknowledged bool
	err := tx.QueryRow(ctx, `INSERT INTO instance.instance_quota_release_outbox
		(tenant_id,instance_id,event_id,original_create_operation_id,reason,cleanup_completed_at,cleanup_version)
		VALUES($1,$2,$3,$4,'RESOURCE_RELEASED',$5,$6)
		ON CONFLICT(tenant_id,instance_id) DO UPDATE SET
		cleanup_completed_at=COALESCE(instance.instance_quota_release_outbox.cleanup_completed_at,EXCLUDED.cleanup_completed_at),
		cleanup_version=COALESCE(instance.instance_quota_release_outbox.cleanup_version,EXCLUDED.cleanup_version)
		RETURNING event_id::text,original_create_operation_id::text,reason,acknowledged`,
		i.TenantID, i.ID, releaseEventID(*i), i.CreateOperationID, i.CleanupCompletedAt, i.Version).Scan(&event, &create, &reason, &acknowledged)
	if err != nil {
		return err
	}
	if event != releaseEventID(*i) || create != i.CreateOperationID || reason != "RESOURCE_RELEASED" {
		return biz.ErrConflict
	}
	if acknowledged {
		i.QuotaReturned = true
		i.Reason = ""
	}
	return nil
}

// Dispatch claims one due event. Failed or crashed senders are retried after
// five seconds, behind other due events, without consulting Kubernetes or ACC.
func (q *Quota) Dispatch(ctx context.Context) (bool, error) {
	var tenant, id string
	err := q.repo.pool.QueryRow(ctx, `WITH candidate AS (
		SELECT tenant_id,instance_id FROM instance.instance_quota_release_outbox
		WHERE NOT acknowledged AND cleanup_completed_at IS NOT NULL AND next_attempt_at<=clock_timestamp()
		ORDER BY next_attempt_at,event_id FOR UPDATE SKIP LOCKED LIMIT 1
	) UPDATE instance.instance_quota_release_outbox AS o
		SET next_attempt_at=clock_timestamp()+interval '5 seconds'
		FROM candidate c WHERE o.tenant_id=c.tenant_id AND o.instance_id=c.instance_id
		RETURNING o.tenant_id::text,o.instance_id::text`).Scan(&tenant, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	i, err := q.repo.Get(ctx, tenant, id)
	if err != nil {
		return true, err
	}
	return true, q.Report(ctx, i)
}

// Report only delivers a committed event. Caller-supplied cleanup flags never
// create an event or establish authority to refund the original ledger.
func (q *Quota) Report(ctx context.Context, ref biz.Instance) error {
	i, err := q.repo.Get(ctx, ref.TenantID, ref.ID)
	if err != nil {
		return err
	}
	if ref.CreateOperationID != i.CreateOperationID || !biz.OwnerCleanupReady(i) || i.CleanupCompletedAt.IsZero() {
		return biz.ErrInvalid
	}
	if i.Spec.ComputeType == biz.TypeCPU {
		if !i.Spec.GPU.Empty() {
			return biz.ErrInvalid
		}
		return nil
	}
	var event, create, reason string
	var acknowledged bool
	var completed *time.Time
	var version *int64
	err = q.repo.pool.QueryRow(ctx, `SELECT event_id::text,original_create_operation_id::text,reason,acknowledged,cleanup_completed_at,cleanup_version
		FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2`, i.TenantID, i.ID).Scan(&event, &create, &reason, &acknowledged, &completed, &version)
	if err != nil {
		return err
	}
	if event != releaseEventID(i) || create != i.CreateOperationID || reason != "RESOURCE_RELEASED" || completed == nil || !completed.Equal(i.CleanupCompletedAt) || version == nil || *version > i.Version {
		return biz.ErrConflict
	}
	if acknowledged {
		return nil
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	reply, err := q.client.ReportResourceReleased(call, &quota.ReportResourceReleasedRequest{ReleaseEventId: event, CreateOperationId: create, ResourceId: i.ID, Reason: quota.ReleaseReason_RESOURCE_RELEASED})
	if err != nil {
		return err
	}
	if reply == nil || len(reply.ProtoReflect().GetUnknown()) != 0 || reply.ReleaseEventId != event || reply.CreateOperationId != create || reply.ResourceId != i.ID || (reply.Status != quota.ResourceReleaseStatus_APPLIED && reply.Status != quota.ResourceReleaseStatus_ALREADY_APPLIED) {
		return fmt.Errorf("invalid resource release receipt")
	}
	return q.acknowledge(ctx, i, event)
}

func (q *Quota) acknowledge(ctx context.Context, sent biz.Instance, event string) error {
	tx, err := q.repo.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	i, err := scanInstance(tx.QueryRow(ctx, instanceSelect+" FOR UPDATE", sent.TenantID, sent.ID))
	if err != nil {
		return err
	}
	if !biz.OwnerCleanupReady(i) || i.CreateOperationID != sent.CreateOperationID || !i.CleanupCompletedAt.Equal(sent.CleanupCompletedAt) {
		return biz.ErrConflict
	}
	result, err := tx.Exec(ctx, `UPDATE instance.instance_quota_release_outbox SET acknowledged=true
		WHERE tenant_id=$1 AND instance_id=$2 AND event_id=$3 AND original_create_operation_id=$4
		AND reason='RESOURCE_RELEASED' AND cleanup_completed_at=$5`, i.TenantID, i.ID, event, i.CreateOperationID, i.CleanupCompletedAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return biz.ErrConflict
	}
	i.QuotaReturned = true
	i.Observed = biz.Deleted
	i.Reason = ""
	raw, err := json.Marshal(i)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE instance.instances SET snapshot=$3,updated_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID, raw); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE instance.instance_operations SET state='completed',reason='' WHERE tenant_id=$1 AND instance_id=$2 AND operation_id=$3 AND action='delete'", i.TenantID, i.ID, i.LastOperationID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
