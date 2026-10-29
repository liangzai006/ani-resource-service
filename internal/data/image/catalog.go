package data

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image/sqlcgen"
)

func fromRegistration(v sqlcgen.ImageRegistration, s biz.Space) (biz.Registration, error) {
	if v.SpaceID != s.ID || v.OwnerScope != string(s.Scope) || textValue(v.TenantID) != s.TenantID {
		return biz.Registration{}, biz.Fail(biz.InternalError, "invalid stored image ownership")
	}
	r := biz.Registration{ID: v.ImageID, TenantID: textValue(v.TenantID), SpaceID: v.SpaceID, Scope: biz.ImageScope(v.OwnerScope), Repository: v.Repository, SourceReference: v.SourceReference, Digest: v.Digest, ResolvedReference: s.RegistryAuthority + "/" + s.ProjectName + "/" + v.Repository + "@" + v.Digest, MediaType: v.MediaType, Metadata: biz.Metadata{DisplayName: v.DisplayName, Description: v.Description, Purposes: v.Purposes, Accelerator: v.Accelerator}, Version: v.Version, CreatedBy: v.CreatedBy, UpdatedBy: v.UpdatedBy, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, UnregisteredAt: v.UnregisteredAt}
	if err := decodeTyped(v.Platforms, &r.Platforms); err != nil {
		return biz.Registration{}, err
	}
	return r, nil
}
func (p *Postgres) FindTenantRegistration(ctx context.Context, tenant, id string) (biz.Registration, error) {
	if _, err := biz.ParseTenant(tenant); err != nil {
		return biz.Registration{}, err
	}
	row, err := sqlcgen.New(p.connection(ctx)).GetTenantRegistration(ctx, sqlcgen.GetTenantRegistrationParams{TenantID: &tenant, ImageID: id})
	if err != nil {
		return biz.Registration{}, databaseError(err)
	}
	s, err := p.FindTenantSpace(ctx, tenant)
	if err != nil {
		return biz.Registration{}, err
	}
	return fromRegistration(row, s)
}
func (p *Postgres) FindPlatformRegistration(ctx context.Context, id string) (biz.Registration, error) {
	row, err := sqlcgen.New(p.connection(ctx)).GetPlatformRegistration(ctx, sqlcgen.GetPlatformRegistrationParams{ImageID: id})
	if err != nil {
		return biz.Registration{}, databaseError(err)
	}
	s, err := p.FindPlatformSpace(ctx)
	if err != nil {
		return biz.Registration{}, err
	}
	return fromRegistration(row, s)
}

func (p *Postgres) FindTenantRegistrationsByReference(ctx context.Context, tenant string, ref biz.ReferenceLookup) ([]biz.Registration, error) {
	if _, err := biz.ParseTenant(tenant); err != nil {
		return nil, err
	}
	rows, err := sqlcgen.New(p.connection(ctx)).FindTenantRegistrationsByReference(ctx, sqlcgen.FindTenantRegistrationsByReferenceParams{TenantID: &tenant, SpaceID: ref.SpaceID, Repository: ref.Repository, IsDigest: ref.IsDigest, Reference: ref.Reference})
	if err != nil {
		return nil, databaseError(err)
	}
	if len(rows) == 0 {
		return []biz.Registration{}, nil
	}
	s, err := p.FindTenantSpace(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return registrations(rows, s)
}

func (p *Postgres) FindPlatformRegistrationsByReference(ctx context.Context, ref biz.ReferenceLookup) ([]biz.Registration, error) {
	rows, err := sqlcgen.New(p.connection(ctx)).FindPlatformRegistrationsByReference(ctx, sqlcgen.FindPlatformRegistrationsByReferenceParams{SpaceID: ref.SpaceID, Repository: ref.Repository, IsDigest: ref.IsDigest, Reference: ref.Reference})
	if err != nil {
		return nil, databaseError(err)
	}
	if len(rows) == 0 {
		return []biz.Registration{}, nil
	}
	s, err := p.FindPlatformSpace(ctx)
	if err != nil {
		return nil, err
	}
	return registrations(rows, s)
}
func searchPattern(raw string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(raw) + "%"
}
func pagePosition(key *biz.PageKey) (bool, time.Time, string) {
	if key == nil {
		return false, time.Time{}, ""
	}
	return true, key.CreatedAt, key.ImageID
}
func (p *Postgres) PageTenantRegistrations(ctx context.Context, tenant string, f biz.Filter, key *biz.PageKey) ([]biz.Registration, int64, error) {
	if _, err := biz.ParseTenant(tenant); err != nil {
		return nil, 0, err
	}
	f, err := biz.NormalizeFilter(f)
	if err != nil {
		return nil, 0, err
	}
	tx, err := p.connection(ctx).BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, databaseError(err)
	}
	defer tx.Rollback(ctx)
	q := sqlcgen.New(tx)
	has, after, id := pagePosition(key)
	total, err := q.CountTenantRegistrations(ctx, sqlcgen.CountTenantRegistrationsParams{TenantID: &tenant, SearchText: f.Search, SearchPattern: searchPattern(f.Search), Purposes: f.Purposes, Accelerator: f.Accelerator})
	if err != nil {
		return nil, 0, databaseError(err)
	}
	rows, err := q.ListTenantRegistrations(ctx, sqlcgen.ListTenantRegistrationsParams{TenantID: &tenant, SearchText: f.Search, SearchPattern: searchPattern(f.Search), Purposes: f.Purposes, Accelerator: f.Accelerator, HasCursor: has, AfterCreatedAt: after, AfterImageID: id, FetchLimit: int32(f.Limit + 1)})
	if err != nil {
		return nil, 0, databaseError(err)
	}
	if len(rows) == 0 {
		return []biz.Registration{}, total, nil
	}
	stored, err := q.GetTenantSpace(ctx, sqlcgen.GetTenantSpaceParams{TenantID: &tenant})
	if err != nil {
		return nil, 0, databaseError(err)
	}
	values, err := registrations(rows, fromSpace(stored))
	return values, total, err
}
func (p *Postgres) PagePlatformRegistrations(ctx context.Context, f biz.Filter, key *biz.PageKey) ([]biz.Registration, int64, error) {
	f, err := biz.NormalizeFilter(f)
	if err != nil {
		return nil, 0, err
	}
	tx, err := p.connection(ctx).BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, databaseError(err)
	}
	defer tx.Rollback(ctx)
	q := sqlcgen.New(tx)
	has, after, id := pagePosition(key)
	total, err := q.CountPlatformRegistrations(ctx, sqlcgen.CountPlatformRegistrationsParams{SearchText: f.Search, SearchPattern: searchPattern(f.Search), Purposes: f.Purposes, Accelerator: f.Accelerator})
	if err != nil {
		return nil, 0, databaseError(err)
	}
	rows, err := q.ListPlatformRegistrations(ctx, sqlcgen.ListPlatformRegistrationsParams{SearchText: f.Search, SearchPattern: searchPattern(f.Search), Purposes: f.Purposes, Accelerator: f.Accelerator, HasCursor: has, AfterCreatedAt: after, AfterImageID: id, FetchLimit: int32(f.Limit + 1)})
	if err != nil {
		return nil, 0, databaseError(err)
	}
	if len(rows) == 0 {
		return []biz.Registration{}, total, nil
	}
	stored, err := q.GetPlatformSpace(ctx)
	if err != nil {
		return nil, 0, databaseError(err)
	}
	values, err := registrations(rows, fromSpace(stored))
	return values, total, err
}
func registrations(rows []sqlcgen.ImageRegistration, s biz.Space) ([]biz.Registration, error) {
	out := make([]biz.Registration, 0, len(rows))
	for _, row := range rows {
		r, err := fromRegistration(row, s)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// The row change and redacted command result commit in one short transaction.
// A replay precedes version checks. Provider reads happen before this adapter.
func (p *Postgres) applyTenantCatalog(ctx context.Context, c biz.Command, change func(*sqlcgen.Queries, biz.Space) (biz.Registration, error)) (biz.Registration, error) {
	if _, err := biz.ParseTenant(c.TenantID); err != nil {
		return biz.Registration{}, err
	}
	tx, err := p.connection(ctx).Begin(ctx)
	if err != nil {
		return biz.Registration{}, databaseError(err)
	}
	defer tx.Rollback(ctx)
	q := sqlcgen.New(tx)
	if err = q.LockTenantSpace(ctx, sqlcgen.LockTenantSpaceParams{TenantID: c.TenantID}); err != nil {
		return biz.Registration{}, databaseError(err)
	}
	row, err := q.GetTenantSpace(ctx, sqlcgen.GetTenantSpaceParams{TenantID: &c.TenantID})
	if err != nil {
		return biz.Registration{}, databaseError(err)
	}
	if row.SpaceID != c.SpaceID {
		return biz.Registration{}, biz.Fail(biz.ImageNotFound, "image space not found")
	}
	stored, err := reserveTenantCommand(ctx, q, c)
	if err != nil {
		return biz.Registration{}, err
	}
	if stored.State == "succeeded" {
		r := stored.Result.Registration
		if r == nil || r.TenantID != c.TenantID || r.Scope != biz.TenantImages || r.SpaceID != c.SpaceID {
			return biz.Registration{}, biz.Fail(biz.InternalError, "invalid stored image result")
		}
		return *r, nil
	}
	if stored.State != "pending" {
		return biz.Registration{}, biz.Fail(biz.RequestInProgress, "catalog request is not pending")
	}
	result, err := change(q, fromSpace(row))
	if err != nil {
		return biz.Registration{}, err
	}
	stored.Result = biz.CommandResult{Registration: &result}
	if _, err = completeTenantCommand(ctx, q, stored); err != nil {
		return biz.Registration{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.Registration{}, databaseError(err)
	}
	return result, nil
}
func (p *Postgres) ApplyTenantRegistration(ctx context.Context, c biz.Command, r biz.Registration) (biz.Registration, error) {
	if c.Kind != "register_image" || r.Scope != biz.TenantImages || r.TenantID != c.TenantID || r.SpaceID != c.SpaceID {
		return biz.Registration{}, biz.Fail(biz.InvalidArgument, "invalid registration ownership")
	}
	return p.applyTenantCatalog(ctx, c, func(q *sqlcgen.Queries, s biz.Space) (biz.Registration, error) {
		platforms, err := json.Marshal(r.Platforms)
		if err != nil {
			return biz.Registration{}, biz.Fail(biz.InternalError, "platform encoding failed")
		}
		row, err := q.InsertTenantRegistration(ctx, sqlcgen.InsertTenantRegistrationParams{ImageID: r.ID, SpaceID: r.SpaceID, TenantID: &r.TenantID, DisplayName: r.Metadata.DisplayName, Description: r.Metadata.Description, Repository: r.Repository, SourceReference: r.SourceReference, Digest: r.Digest, MediaType: r.MediaType, Platforms: platforms, Purposes: r.Metadata.Purposes, Accelerator: r.Metadata.Accelerator, Actor: c.Actor})
		if err != nil {
			return biz.Registration{}, databaseError(err)
		}
		return fromRegistration(row, s)
	})
}
func (p *Postgres) ApplyTenantMetadata(ctx context.Context, c biz.Command, r biz.UpdateImage) (biz.Registration, error) {
	if c.Kind != "update_image" || r.TenantID != c.TenantID {
		return biz.Registration{}, biz.Fail(biz.InvalidArgument, "invalid metadata ownership")
	}
	return p.applyTenantCatalog(ctx, c, func(q *sqlcgen.Queries, s biz.Space) (biz.Registration, error) {
		row, err := q.UpdateTenantMetadata(ctx, sqlcgen.UpdateTenantMetadataParams{TenantID: &r.TenantID, ImageID: r.ImageID, DisplayName: r.Metadata.DisplayName, Description: r.Metadata.Description, Purposes: r.Metadata.Purposes, Accelerator: r.Metadata.Accelerator, Actor: c.Actor, ExpectedVersion: r.ExpectedVersion})
		if errors.Is(err, pgx.ErrNoRows) {
			return biz.Registration{}, tenantCASFailure(ctx, q, r.TenantID, r.ImageID)
		}
		if err != nil {
			return biz.Registration{}, databaseError(err)
		}
		return fromRegistration(row, s)
	})
}
func (p *Postgres) ApplyTenantUnregister(ctx context.Context, c biz.Command, r biz.UnregisterImage) (biz.Registration, error) {
	if c.Kind != "unregister_image" || r.TenantID != c.TenantID {
		return biz.Registration{}, biz.Fail(biz.InvalidArgument, "invalid unregister ownership")
	}
	return p.applyTenantCatalog(ctx, c, func(q *sqlcgen.Queries, s biz.Space) (biz.Registration, error) {
		existing, err := q.GetTenantRegistration(ctx, sqlcgen.GetTenantRegistrationParams{TenantID: &r.TenantID, ImageID: r.ImageID})
		if err != nil {
			return biz.Registration{}, databaseError(err)
		}
		if existing.UnregisteredAt != nil {
			return fromRegistration(existing, s)
		}
		row, err := q.UnregisterTenantRegistration(ctx, sqlcgen.UnregisterTenantRegistrationParams{TenantID: &r.TenantID, ImageID: r.ImageID, Actor: c.Actor, ExpectedVersion: r.ExpectedVersion})
		if errors.Is(err, pgx.ErrNoRows) {
			return biz.Registration{}, tenantCASFailure(ctx, q, r.TenantID, r.ImageID)
		}
		if err != nil {
			return biz.Registration{}, databaseError(err)
		}
		return fromRegistration(row, s)
	})
}
func tenantCASFailure(ctx context.Context, q *sqlcgen.Queries, tenant, id string) error {
	row, err := q.GetTenantRegistration(ctx, sqlcgen.GetTenantRegistrationParams{TenantID: &tenant, ImageID: id})
	if err != nil {
		return databaseError(err)
	}
	if row.UnregisteredAt != nil {
		return biz.Fail(biz.ImageNotFound, "image is unregistered")
	}
	return biz.Fail(biz.VersionConflict, "image version changed")
}
