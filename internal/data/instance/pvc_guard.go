package instance

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	"log/slog"
	"time"
)

const guardSelect = `SELECT tenant_id::text,cluster_id::text,namespace,pvc_name,target_uid,COALESCE(ticket_id::text,''),guard_epoch,lease_owner,lease_epoch,CASE WHEN lease_until='-infinity'::timestamptz THEN 'epoch'::timestamptz ELSE lease_until END AS lease_until,phase,write_started,last_write_outcome,last_observation,last_reason,retry_at,created_at,updated_at FROM instance.borrowed_pvc_guards`

func scanGuard(row scanner) (storage.DeleteRecord, error) {
	var r storage.DeleteRecord
	var obs []byte
	err := row.Scan(&r.Identity.TenantID, &r.Identity.ClusterID, &r.Identity.Namespace, &r.Identity.PVCName, &r.Identity.TargetUID, &r.Token.TicketID, &r.Token.GuardEpoch, &r.Token.LeaseOwner, &r.Token.LeaseEpoch, &r.Token.LeaseUntil, &r.Phase, &r.WriteStarted, &r.LastWriteOutcome, &obs, &r.LastReason, &r.RetryAt, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, biz.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if len(obs) > 0 {
		var o storage.DeleteObservation
		if err = json.Unmarshal(obs, &o); err != nil {
			return r, err
		}
		r.LastObservation = o.State
		r.ObservedUID = o.ObservedUID
	}
	return r, nil
}
func (p *Postgres) UseIntentGate(enabled bool) { p.newIntentsEnabled = enabled }
func validPVCIdentity(id storage.PVCIdentity) bool {
	return biz.UUID(id.TenantID) && biz.UUID(id.ClusterID) && id.Namespace != "" && id.PVCName != "" && id.TargetUID != ""
}
func (p *Postgres) ReserveDelete(ctx context.Context, id storage.PVCIdentity, executor string, ttl time.Duration) (storage.DeleteRecord, bool, error) {
	var r storage.DeleteRecord
	if !validPVCIdentity(id) || executor == "" || ttl != 45*time.Second {
		return r, false, storage.ErrInvalid
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return r, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO instance.borrowed_pvc_guards(cluster_id,namespace,pvc_name,tenant_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, id.ClusterID, id.Namespace, id.PVCName, id.TenantID); err != nil {
		return r, false, err
	}
	r, err = scanGuard(tx.QueryRow(ctx, guardSelect+` WHERE cluster_id=$1 AND namespace=$2 AND pvc_name=$3 FOR UPDATE`, id.ClusterID, id.Namespace, id.PVCName))
	if err != nil {
		return r, false, err
	}
	if r.Identity.TenantID != id.TenantID {
		return r, false, storage.ErrNotFound
	}
	active := r.Phase == "reserved" || r.Phase == "deleting" || r.Phase == "observing"
	if active {
		if r.Identity.TargetUID != id.TargetUID {
			return r, false, storage.ErrIdentityChanged
		}
		var live bool
		if err = tx.QueryRow(ctx, `SELECT lease_until>clock_timestamp() FROM instance.borrowed_pvc_guards WHERE cluster_id=$1 AND namespace=$2 AND pvc_name=$3`, id.ClusterID, id.Namespace, id.PVCName).Scan(&live); err != nil {
			return r, false, err
		}
		if live {
			return r, false, nil
		}
		r, err = scanGuard(tx.QueryRow(ctx, `UPDATE instance.borrowed_pvc_guards SET lease_owner=$4,lease_epoch=lease_epoch+1,lease_until=clock_timestamp()+interval '45 seconds',updated_at=clock_timestamp() WHERE cluster_id=$1 AND namespace=$2 AND pvc_name=$3 RETURNING `+guardSelect[len("SELECT "):len(guardSelect)-len(" FROM instance.borrowed_pvc_guards")], id.ClusterID, id.Namespace, id.PVCName, executor))
		if err != nil {
			return r, false, err
		}
		return r, true, tx.Commit(ctx)
	}
	if r.Phase == "completed" && r.Token.TicketID != "" && r.Identity.TargetUID == id.TargetUID {
		return r, false, nil
	}
	if !p.newIntentsEnabled {
		return r, false, storage.ErrBackendUnavailable
	}
	var used bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM instance.borrowed_volume_references WHERE cluster_id=$1 AND namespace=$2 AND pvc_name=$3 AND state IN ('pending','active','retiring'))`, id.ClusterID, id.Namespace, id.PVCName).Scan(&used); err != nil {
		return r, false, err
	}
	if used {
		p.pvcReferenceConflicts.Add(1)
		return r, false, storage.ErrVolumeInUse
	}
	r, err = scanGuard(tx.QueryRow(ctx, `UPDATE instance.borrowed_pvc_guards SET target_uid=$4,ticket_id=$5,guard_epoch=guard_epoch+1,phase='reserved',write_started=false,lease_owner=$6,lease_epoch=lease_epoch+1,lease_until=clock_timestamp()+interval '45 seconds',last_write_outcome='not_sent',last_observation=NULL,last_reason='',retry_at=clock_timestamp(),created_at=clock_timestamp(),updated_at=clock_timestamp() WHERE cluster_id=$1 AND namespace=$2 AND pvc_name=$3 RETURNING `+guardSelect[len("SELECT "):len(guardSelect)-len(" FROM instance.borrowed_pvc_guards")], id.ClusterID, id.Namespace, id.PVCName, id.TargetUID, uuid.NewString(), executor))
	if err != nil {
		return r, false, err
	}
	return r, true, tx.Commit(ctx)
}
func (p *Postgres) ClaimDeleteRecovery(ctx context.Context, executor string, ttl time.Duration) (*storage.DeleteRecord, error) {
	if executor == "" || ttl != 45*time.Second {
		return nil, storage.ErrInvalid
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var cluster, ns, name string
	err = tx.QueryRow(ctx, `SELECT cluster_id::text,namespace,pvc_name FROM instance.borrowed_pvc_guards WHERE phase IN ('reserved','deleting','observing') AND retry_at<=clock_timestamp() AND lease_until<clock_timestamp() ORDER BY retry_at,cluster_id,namespace,pvc_name FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&cluster, &ns, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := scanGuard(tx.QueryRow(ctx, `UPDATE instance.borrowed_pvc_guards SET lease_owner=$4,lease_epoch=lease_epoch+1,lease_until=clock_timestamp()+interval '45 seconds',updated_at=clock_timestamp() WHERE cluster_id=$1 AND namespace=$2 AND pvc_name=$3 RETURNING `+guardSelect[len("SELECT "):len(guardSelect)-len(" FROM instance.borrowed_pvc_guards")], cluster, ns, name, executor))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &r, nil
}

const tokenWhere = ` WHERE tenant_id=$1 AND cluster_id=$2 AND namespace=$3 AND pvc_name=$4 AND target_uid=$5 AND ticket_id=$6 AND guard_epoch=$7 AND lease_owner=$8 AND lease_epoch=$9 AND lease_until=$10 AND lease_until>clock_timestamp() AND phase IN ('reserved','deleting','observing')`

func tokenArgs(id storage.PVCIdentity, t storage.DeleteToken) []any {
	return []any{id.TenantID, id.ClusterID, id.Namespace, id.PVCName, id.TargetUID, t.TicketID, t.GuardEpoch, t.LeaseOwner, t.LeaseEpoch, t.LeaseUntil}
}
func (p *Postgres) guardCAS(ctx context.Context, id storage.PVCIdentity, t storage.DeleteToken, set, extra string, values ...any) error {
	args := append(tokenArgs(id, t), values...)
	tag, err := p.pool.Exec(ctx, `UPDATE instance.borrowed_pvc_guards SET `+set+`,updated_at=clock_timestamp()`+tokenWhere+extra, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return storage.ErrDeleteLease
	}
	return nil
}
func (p *Postgres) MarkDeleteWriteStarted(ctx context.Context, id storage.PVCIdentity, t storage.DeleteToken) error {
	return p.guardCAS(ctx, id, t, `phase='deleting',write_started=true`, "")
}
func (p *Postgres) RecordDeleteObservation(ctx context.Context, id storage.PVCIdentity, t storage.DeleteToken, o storage.DeleteObservation) error {
	if o.State == "absent" || o.State == "identity_changed" {
		return storage.ErrInvalid
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	err = p.guardCAS(ctx, id, t, `phase=CASE WHEN write_started THEN 'observing' ELSE 'reserved' END,last_observation=$11,last_write_outcome=$12,last_reason=$13,retry_at=$14,lease_owner='',lease_until='-infinity'`, "", raw, o.WriteOutcome, o.Reason, o.RetryAt)
	if err == nil && o.Reason != "" {
		slog.Warn("PVC delete recovery pending", "ticket_id", t.TicketID, "guard_epoch", t.GuardEpoch, "lease_epoch", t.LeaseEpoch, "tenant_id", id.TenantID, "namespace", id.Namespace, "pvc_name", id.PVCName, "reason", o.Reason, "write_outcome", o.WriteOutcome)
	}
	return err
}
func (p *Postgres) CancelUnsentDelete(ctx context.Context, id storage.PVCIdentity, t storage.DeleteToken, reason string) error {
	return p.guardCAS(ctx, id, t, `phase='cancelled',last_reason=$11,lease_owner='',lease_until='-infinity'`, ` AND write_started=false`, reason)
}
func (p *Postgres) CompleteDelete(ctx context.Context, id storage.PVCIdentity, t storage.DeleteToken, o storage.DeleteObservation) error {
	if (o.State != "absent" && o.State != "identity_changed") || (o.State == "identity_changed" && (o.ObservedUID == "" || o.ObservedUID == id.TargetUID)) {
		return storage.ErrInvalid
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return p.guardCAS(ctx, id, t, `phase='completed',last_observation=$11,last_write_outcome=$12,last_reason=$13,lease_owner='',lease_until='-infinity'`, "", raw, o.WriteOutcome, o.Reason)
}

var _ storage.BorrowedPVCGuard = (*Postgres)(nil)

func (p *Postgres) PVCDeleting(ctx context.Context, tenant, cluster, namespace, name string) (bool, error) {
	var owner, phase string
	err := p.pool.QueryRow(ctx, "SELECT tenant_id::text,phase FROM instance.borrowed_pvc_guards WHERE cluster_id=$1 AND namespace=$2 AND pvc_name=$3", cluster, namespace, name).Scan(&owner, &phase)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if owner != tenant {
		return false, biz.ErrDenied
	}
	return phase == "reserved" || phase == "deleting" || phase == "observing", nil
}
