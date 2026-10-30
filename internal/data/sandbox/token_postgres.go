package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
)

const tokenSelect = `SELECT claims,revoked_at IS NOT NULL,request_digest FROM sandbox.access_tokens`

func scanToken(row interface{ Scan(...any) error }) (biz.TokenRecord, string, error) {
	var result biz.TokenRecord
	var raw []byte
	var digest string
	if err := row.Scan(&raw, &result.Revoked, &digest); err != nil {
		return result, "", err
	}
	if err := json.Unmarshal(raw, &result.Claims); err != nil {
		return result, "", err
	}
	return result, digest, nil
}
func (p *Postgres) AcceptToken(ctx context.Context, c biz.TokenCommand, keyID string, now time.Time) (biz.TokenRecord, error) {
	if err := biz.RequireTenant(ctx, c.TenantID); err != nil {
		return biz.TokenRecord{}, err
	}
	digest, err := biz.TokenDigest(c)
	if err != nil {
		return biz.TokenRecord{}, err
	}
	if keyID == "" || now.IsZero() {
		return biz.TokenRecord{}, biz.ErrInvalid
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.TokenRecord{}, err
	}
	defer tx.Rollback(ctx)
	s, err := lockCodeRunSandbox(ctx, tx, c.TenantID, c.SandboxID)
	if err != nil {
		return biz.TokenRecord{}, err
	}
	if _, err = biz.DataPlaneTargetForSandbox(s, biz.DataPlaneCodeRun); err != nil {
		return biz.TokenRecord{}, err
	}
	old, oldDigest, err := scanToken(tx.QueryRow(ctx, tokenSelect+" WHERE tenant_id=$1 AND sandbox_id=$2 AND idempotency_key=$3", c.TenantID, c.SandboxID, c.IdempotencyKey))
	if err == nil {
		if oldDigest != digest || old.Claims.Version != s.Version {
			return biz.TokenRecord{}, biz.ErrConflict
		}
		old.Replayed = true
		return old, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return biz.TokenRecord{}, err
	}
	now = now.UTC().Truncate(time.Second)
	claim := biz.TokenClaims{KeyID: keyID, TenantID: c.TenantID, SandboxID: c.SandboxID, Version: s.Version, TokenID: c.TokenID, GovTenantID: c.GovTenantID, IssuerID: c.IssuerID, Scopes: c.Scopes, IssuedAt: now.Unix(), ExpiresAt: now.Add(c.ExpiresIn).Unix()}
	raw, err := json.Marshal(claim)
	if err != nil {
		return biz.TokenRecord{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO sandbox.access_tokens(tenant_id,sandbox_id,token_id,idempotency_key,request_digest,claims) VALUES($1,$2,$3,$4,$5,$6)`, c.TenantID, c.SandboxID, c.TokenID, c.IdempotencyKey, digest, raw)
	if err != nil {
		return biz.TokenRecord{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.TokenRecord{}, err
	}
	return biz.TokenRecord{Claims: claim}, nil
}
func (p *Postgres) GetToken(ctx context.Context, tenant, id, tokenID string) (biz.TokenRecord, error) {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return biz.TokenRecord{}, err
	}
	if !biz.UUID(id) || !biz.UUID(tokenID) {
		return biz.TokenRecord{}, biz.ErrInvalid
	}
	// A token becomes invalid immediately when the lifecycle version changes or
	// the Sandbox ceases to be running. The token itself is never stored.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.TokenRecord{}, err
	}
	defer tx.Rollback(ctx)
	sandbox, err := lockCodeRunSandbox(ctx, tx, tenant, id)
	if err != nil {
		return biz.TokenRecord{}, err
	}
	if _, err = biz.DataPlaneTargetForSandbox(sandbox, biz.DataPlaneCodeRun); err != nil {
		return biz.TokenRecord{}, err
	}
	record, _, err := scanToken(tx.QueryRow(ctx, tokenSelect+" WHERE tenant_id=$1 AND sandbox_id=$2 AND token_id=$3", tenant, id, tokenID))
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.TokenRecord{}, biz.ErrNotFound
	}
	if err != nil {
		return biz.TokenRecord{}, err
	}
	if record.Claims.Version != sandbox.Version {
		return biz.TokenRecord{}, biz.ErrTokenInvalid
	}
	return record, tx.Commit(ctx)
}
func (p *Postgres) RevokeToken(ctx context.Context, tenant, id, tokenID string) error {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return err
	}
	if !biz.UUID(id) || !biz.UUID(tokenID) {
		return biz.ErrInvalid
	}
	tag, err := p.pool.Exec(ctx, `UPDATE sandbox.access_tokens SET revoked_at=COALESCE(revoked_at,clock_timestamp()) WHERE tenant_id=$1 AND sandbox_id=$2 AND token_id=$3`, tenant, id, tokenID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return biz.ErrNotFound
	}
	return nil
}
