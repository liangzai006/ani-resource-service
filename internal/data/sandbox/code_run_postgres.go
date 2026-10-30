package sandbox

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
)

// lockCodeRunSandbox follows the same work-row then sandbox-row lock order as
// lifecycle Change, so code-run acceptance and pause/delete are serialized.
func lockCodeRunSandbox(ctx context.Context, tx pgx.Tx, tenant, id string) (biz.Sandbox, error) {
	var marker int
	if err := tx.QueryRow(ctx, "SELECT 1 FROM sandbox.work WHERE tenant_id=$1 AND sandbox_id=$2 FOR UPDATE", tenant, id).Scan(&marker); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return biz.Sandbox{}, biz.ErrNotFound
		}
		return biz.Sandbox{}, err
	}
	s, err := scanSandbox(tx.QueryRow(ctx, sandboxSelect+" WHERE s.tenant_id=$1 AND s.sandbox_id=$2 FOR UPDATE OF s", tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Sandbox{}, biz.ErrNotFound
	}
	return s, err
}

func scanCodeRun(row interface{ Scan(...any) error }) (biz.CodeRunIntent, string, error) {
	var intent biz.CodeRunIntent
	var target []byte
	var digest string
	err := row.Scan(&intent.TenantID, &intent.SandboxID, &intent.RunID, &digest, &target, &intent.State, &intent.Reason, &intent.ExitCode, &intent.Stdout, &intent.Stderr, &intent.Truncated)
	if err != nil {
		return intent, "", err
	}
	if err = json.Unmarshal(target, &intent.Target); err != nil {
		return intent, "", err
	}
	return intent, digest, nil
}

const codeRunSelect = `SELECT tenant_id::text,sandbox_id::text,run_id::text,request_digest,target,state,reason,exit_code,stdout,stderr,truncated FROM sandbox.code_runs`

// AcceptCodeRun records only a digest and frozen target, never source or stdin.
// Replaying a key returns its original run even after a lifecycle transition.
func (p *Postgres) AcceptCodeRun(ctx context.Context, command biz.CodeRunCommand) (biz.CodeRunIntent, error) {
	if err := biz.RequireTenant(ctx, command.TenantID); err != nil {
		return biz.CodeRunIntent{}, err
	}
	digest, err := biz.CodeRunRequestDigest(command)
	if err != nil {
		return biz.CodeRunIntent{}, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.CodeRunIntent{}, err
	}
	defer tx.Rollback(ctx)
	s, err := lockCodeRunSandbox(ctx, tx, command.TenantID, command.SandboxID)
	if err != nil {
		return biz.CodeRunIntent{}, err
	}
	old, oldDigest, err := scanCodeRun(tx.QueryRow(ctx, codeRunSelect+" WHERE tenant_id=$1 AND sandbox_id=$2 AND idempotency_key=$3", command.TenantID, command.SandboxID, command.IdempotencyKey))
	if err == nil {
		if oldDigest != digest {
			return biz.CodeRunIntent{}, biz.ErrConflict
		}
		old.Replayed = true
		return old, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return biz.CodeRunIntent{}, err
	}
	if command.ExpectedVersion > 0 && command.ExpectedVersion != s.Version {
		return biz.CodeRunIntent{}, biz.ErrVersion
	}
	target, err := biz.DataPlaneTargetForSandbox(s, biz.DataPlaneCodeRun)
	if err != nil {
		return biz.CodeRunIntent{}, err
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return biz.CodeRunIntent{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO sandbox.code_runs(tenant_id,sandbox_id,run_id,idempotency_key,request_digest,target,state) VALUES($1,$2,$3,$4,$5,$6,'accepted')`, command.TenantID, command.SandboxID, command.RunID, command.IdempotencyKey, digest, raw)
	if err != nil {
		return biz.CodeRunIntent{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.CodeRunIntent{}, err
	}
	return biz.CodeRunIntent{TenantID: command.TenantID, SandboxID: command.SandboxID, RunID: command.RunID, State: "accepted", Target: target}, nil
}

// BeginCodeRunDispatch can succeed at most once. A committed dispatching row
// must never be sent again, including after timeout, restart or lost response.
func (p *Postgres) BeginCodeRunDispatch(ctx context.Context, command biz.CodeRunCommand) (biz.CodeRunIntent, bool, error) {
	if err := biz.RequireTenant(ctx, command.TenantID); err != nil {
		return biz.CodeRunIntent{}, false, err
	}
	digest, err := biz.CodeRunRequestDigest(command)
	if err != nil {
		return biz.CodeRunIntent{}, false, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.CodeRunIntent{}, false, err
	}
	defer tx.Rollback(ctx)
	s, err := lockCodeRunSandbox(ctx, tx, command.TenantID, command.SandboxID)
	if err != nil {
		return biz.CodeRunIntent{}, false, err
	}
	intent, storedDigest, err := scanCodeRun(tx.QueryRow(ctx, codeRunSelect+" WHERE tenant_id=$1 AND sandbox_id=$2 AND run_id=$3 AND idempotency_key=$4 FOR UPDATE", command.TenantID, command.SandboxID, command.RunID, command.IdempotencyKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.CodeRunIntent{}, false, biz.ErrNotFound
	}
	if err != nil {
		return biz.CodeRunIntent{}, false, err
	}
	if storedDigest != digest {
		return biz.CodeRunIntent{}, false, biz.ErrConflict
	}
	if intent.State != "accepted" {
		return intent, false, tx.Commit(ctx)
	}
	current, targetErr := biz.DataPlaneTargetForSandbox(s, biz.DataPlaneCodeRun)
	if targetErr != nil || current != intent.Target {
		if _, err = tx.Exec(ctx, `UPDATE sandbox.code_runs SET state='cancelled',reason='SANDBOX_LIFECYCLE_CHANGED',updated_at=clock_timestamp() WHERE tenant_id=$1 AND run_id=$2`, command.TenantID, command.RunID); err != nil {
			return biz.CodeRunIntent{}, false, err
		}
		intent.State = "cancelled"
		intent.Reason = "SANDBOX_LIFECYCLE_CHANGED"
		return intent, false, tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `UPDATE sandbox.code_runs SET state='dispatching',updated_at=clock_timestamp() WHERE tenant_id=$1 AND run_id=$2`, command.TenantID, command.RunID); err != nil {
		return biz.CodeRunIntent{}, false, err
	}
	intent.State = "dispatching"
	if err = tx.Commit(ctx); err != nil {
		return biz.CodeRunIntent{}, false, err
	}
	return intent, true, nil
}

func (p *Postgres) FailCodeRunBeforeDispatch(ctx context.Context, tenant, sandboxID, runID string) error {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return err
	}
	if !biz.UUID(sandboxID) || !biz.UUID(runID) {
		return biz.ErrInvalid
	}
	result, err := p.pool.Exec(ctx, `UPDATE sandbox.code_runs SET state='failed',reason='SANDBOX_TARGET_UNAVAILABLE',updated_at=clock_timestamp() WHERE tenant_id=$1 AND sandbox_id=$2 AND run_id=$3 AND state='accepted'`, tenant, sandboxID, runID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return biz.ErrConflict
	}
	return nil
}

func (p *Postgres) GetCodeRun(ctx context.Context, tenant, sandboxID, runID string) (biz.CodeRunIntent, error) {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return biz.CodeRunIntent{}, err
	}
	if !biz.UUID(sandboxID) || !biz.UUID(runID) {
		return biz.CodeRunIntent{}, biz.ErrInvalid
	}
	// No receipt after the maximum dispatch window cannot establish whether
	// the command ran. Preserve an explicit unknown state, never re-dispatch.
	if _, err := p.pool.Exec(ctx, `UPDATE sandbox.code_runs SET state='unknown',reason='SANDBOX_DISPATCH_OUTCOME_UNKNOWN',updated_at=clock_timestamp() WHERE tenant_id=$1 AND sandbox_id=$2 AND run_id=$3 AND state='dispatching' AND updated_at < clock_timestamp()-interval '320 seconds'`, tenant, sandboxID, runID); err != nil {
		return biz.CodeRunIntent{}, err
	}
	intent, _, err := scanCodeRun(p.pool.QueryRow(ctx, codeRunSelect+" WHERE tenant_id=$1 AND sandbox_id=$2 AND run_id=$3", tenant, sandboxID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.CodeRunIntent{}, biz.ErrNotFound
	}
	return intent, err
}

// FinishCodeRun accepts a confirmed exit or marks an unconfirmed dispatch
// unknown. Neither state is eligible for automatic dispatch again.
func (p *Postgres) FinishCodeRun(ctx context.Context, tenant, sandboxID, runID string, exitCode *int, stdout, stderr []byte, truncated bool) error {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return err
	}
	if !biz.UUID(sandboxID) || !biz.UUID(runID) || len(stdout) > biz.CodeRunOutputBytes || len(stderr) > biz.CodeRunOutputBytes || (exitCode != nil && (*exitCode < 0 || *exitCode > 255)) {
		return biz.ErrInvalid
	}
	state := "unknown"
	if exitCode != nil {
		state = "failed"
		if *exitCode == 0 {
			state = "succeeded"
		}
	}
	if stdout == nil {
		stdout = []byte{}
	}
	if stderr == nil {
		stderr = []byte{}
	}
	reason := ""
	if state == "unknown" {
		reason = "SANDBOX_DISPATCH_OUTCOME_UNKNOWN"
	}
	result, err := p.pool.Exec(ctx, `UPDATE sandbox.code_runs SET state=$4,exit_code=$5,stdout=$6,stderr=$7,truncated=$8,reason=$9,updated_at=clock_timestamp() WHERE tenant_id=$1 AND sandbox_id=$2 AND run_id=$3 AND state='dispatching'`, tenant, sandboxID, runID, state, exitCode, stdout, stderr, truncated, reason)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return biz.ErrConflict
	}
	return nil
}
