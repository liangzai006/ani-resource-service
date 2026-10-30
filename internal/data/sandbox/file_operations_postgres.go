package sandbox

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
)

const fileOperationSelect = `SELECT tenant_id::text,sandbox_id::text,operation_id::text,request_digest,target,state,reason,result FROM sandbox.file_operations`

func scanFileOperation(row interface{ Scan(...any) error }) (biz.FileIntent, string, error) {
	var intent biz.FileIntent
	var digest string
	var target, result []byte
	if err := row.Scan(&intent.TenantID, &intent.SandboxID, &intent.OperationID, &digest, &target, &intent.State, &intent.Reason, &result); err != nil {
		return intent, "", err
	}
	if err := json.Unmarshal(target, &intent.Target); err != nil {
		return intent, "", err
	}
	if err := json.Unmarshal(result, &intent.Result); err != nil {
		return intent, "", err
	}
	return intent, digest, nil
}
func (p *Postgres) AcceptFileOperation(ctx context.Context, c biz.FileCommand) (biz.FileIntent, error) {
	if err := biz.RequireTenant(ctx, c.TenantID); err != nil {
		return biz.FileIntent{}, err
	}
	digest, err := biz.FileRequestDigest(c)
	if err != nil {
		return biz.FileIntent{}, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.FileIntent{}, err
	}
	defer tx.Rollback(ctx)
	s, err := lockCodeRunSandbox(ctx, tx, c.TenantID, c.SandboxID)
	if err != nil {
		return biz.FileIntent{}, err
	}
	old, oldDigest, err := scanFileOperation(tx.QueryRow(ctx, fileOperationSelect+" WHERE tenant_id=$1 AND sandbox_id=$2 AND idempotency_key=$3", c.TenantID, c.SandboxID, c.IdempotencyKey))
	if err == nil {
		if oldDigest != digest {
			return biz.FileIntent{}, biz.ErrConflict
		}
		old.Replayed = true
		return old, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return biz.FileIntent{}, err
	}
	if c.ExpectedVersion > 0 && c.ExpectedVersion != s.Version {
		return biz.FileIntent{}, biz.ErrVersion
	}
	target, err := biz.DataPlaneTargetForSandbox(s, biz.DataPlaneFiles)
	if err != nil {
		return biz.FileIntent{}, err
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return biz.FileIntent{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO sandbox.file_operations(tenant_id,sandbox_id,operation_id,idempotency_key,request_digest,target,state) VALUES($1,$2,$3,$4,$5,$6,'accepted')`, c.TenantID, c.SandboxID, c.OperationID, c.IdempotencyKey, digest, raw)
	if err != nil {
		return biz.FileIntent{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.FileIntent{}, err
	}
	return biz.FileIntent{TenantID: c.TenantID, SandboxID: c.SandboxID, OperationID: c.OperationID, State: "accepted", Target: target}, nil
}
func (p *Postgres) BeginFileOperation(ctx context.Context, c biz.FileCommand) (biz.FileIntent, bool, error) {
	if err := biz.RequireTenant(ctx, c.TenantID); err != nil {
		return biz.FileIntent{}, false, err
	}
	digest, err := biz.FileRequestDigest(c)
	if err != nil {
		return biz.FileIntent{}, false, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.FileIntent{}, false, err
	}
	defer tx.Rollback(ctx)
	s, err := lockCodeRunSandbox(ctx, tx, c.TenantID, c.SandboxID)
	if err != nil {
		return biz.FileIntent{}, false, err
	}
	intent, stored, err := scanFileOperation(tx.QueryRow(ctx, fileOperationSelect+" WHERE tenant_id=$1 AND sandbox_id=$2 AND operation_id=$3 AND idempotency_key=$4 FOR UPDATE", c.TenantID, c.SandboxID, c.OperationID, c.IdempotencyKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.FileIntent{}, false, biz.ErrNotFound
	}
	if err != nil {
		return biz.FileIntent{}, false, err
	}
	if stored != digest {
		return biz.FileIntent{}, false, biz.ErrConflict
	}
	if intent.State != "accepted" {
		return intent, false, tx.Commit(ctx)
	}
	current, targetErr := biz.DataPlaneTargetForSandbox(s, biz.DataPlaneFiles)
	if targetErr != nil || current != intent.Target {
		_, err = tx.Exec(ctx, `UPDATE sandbox.file_operations SET state='cancelled',reason='SANDBOX_LIFECYCLE_CHANGED',updated_at=clock_timestamp() WHERE tenant_id=$1 AND operation_id=$2`, c.TenantID, c.OperationID)
		if err != nil {
			return biz.FileIntent{}, false, err
		}
		intent.State = "cancelled"
		intent.Reason = "SANDBOX_LIFECYCLE_CHANGED"
		return intent, false, tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `UPDATE sandbox.file_operations SET state='dispatching',updated_at=clock_timestamp() WHERE tenant_id=$1 AND operation_id=$2`, c.TenantID, c.OperationID)
	if err != nil {
		return biz.FileIntent{}, false, err
	}
	intent.State = "dispatching"
	return intent, true, tx.Commit(ctx)
}
func (p *Postgres) FinishFileOperation(ctx context.Context, tenant, id, operation, reason string, result biz.FileResult) error {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return err
	}
	if !biz.UUID(id) || !biz.UUID(operation) {
		return biz.ErrInvalid
	}
	state := "succeeded"
	allowed := "dispatching"
	switch reason {
	case "":
	case "unknown":
		state = "unknown"
		reason = "SANDBOX_FILE_OUTCOME_UNKNOWN"
	case "invalid", "conflict", "not_found", "too_large":
		state = "failed"
	case "target_unavailable":
		state = "failed"
		allowed = "accepted"
	default:
		return biz.ErrInvalid
	}
	// A file mutation never stores file contents or listing output.
	if result.ContentBase64 != "" || len(result.Items) > 0 {
		return biz.ErrInvalid
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `UPDATE sandbox.file_operations SET state=$4,reason=$5,result=$6,updated_at=clock_timestamp() WHERE tenant_id=$1 AND sandbox_id=$2 AND operation_id=$3 AND state=$7`, tenant, id, operation, state, reason, raw, allowed)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return biz.ErrConflict
	}
	return nil
}
func (p *Postgres) GetFileOperation(ctx context.Context, tenant, id, operation string) (biz.FileIntent, error) {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return biz.FileIntent{}, err
	}
	if !biz.UUID(id) || !biz.UUID(operation) {
		return biz.FileIntent{}, biz.ErrInvalid
	}
	_, err := p.pool.Exec(ctx, `UPDATE sandbox.file_operations SET state='unknown',reason='SANDBOX_FILE_OUTCOME_UNKNOWN',updated_at=clock_timestamp() WHERE tenant_id=$1 AND sandbox_id=$2 AND operation_id=$3 AND state='dispatching' AND updated_at < clock_timestamp()-interval '75 seconds'`, tenant, id, operation)
	if err != nil {
		return biz.FileIntent{}, err
	}
	result, _, err := scanFileOperation(p.pool.QueryRow(ctx, fileOperationSelect+" WHERE tenant_id=$1 AND sandbox_id=$2 AND operation_id=$3", tenant, id, operation))
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.FileIntent{}, biz.ErrNotFound
	}
	return result, err
}
