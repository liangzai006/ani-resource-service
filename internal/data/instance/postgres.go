package instance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance/sqlcgen"
	migrations "github.com/zhangzhe-ctrl/ani-resource-service/migrations/instance"
)

type Postgres struct{ pool *pgxpool.Pool }

func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	p, e := pgxpool.New(ctx, dsn)
	if e != nil {
		return nil, fmt.Errorf("instance database configuration invalid")
	}
	if e = p.Ping(ctx); e != nil {
		p.Close()
		return nil, fmt.Errorf("instance database unavailable")
	}
	r := &Postgres{p}
	if e = r.CheckReady(ctx); e != nil {
		p.Close()
		return nil, e
	}
	return r, nil
}
func (p *Postgres) Close() { p.pool.Close() }
func (p *Postgres) CheckReady(ctx context.Context) error {
	if e := p.checkInstanceSchema(ctx); e != nil {
		return e
	}
	var checksum string
	body, e := migrations.Files.ReadFile("001_instance.sql")
	if e != nil {
		return e
	}
	hash := sha256.Sum256(body)
	if e = p.pool.QueryRow(ctx, "SELECT checksum FROM instance.schema_version WHERE version=1").Scan(&checksum); e != nil || checksum != hex.EncodeToString(hash[:]) {
		return fmt.Errorf("instance schema not migrated or checksum differs")
	}
	return nil
}

// Migrate is an explicit owner command; startup never applies migrations.
func Migrate(ctx context.Context, dsn, role string) error {
	if role == "" {
		return biz.ErrInvalid
	}
	p, e := pgxpool.New(ctx, dsn)
	if e != nil {
		return fmt.Errorf("invalid instance migration configuration")
	}
	defer p.Close()
	tx, e := p.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(84015001)"); e != nil {
		return e
	}
	var current string
	var exists bool
	if e = tx.QueryRow(ctx, "SELECT current_user,EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", role).Scan(&current, &exists); e != nil {
		return e
	}
	if current == role || !exists {
		return fmt.Errorf("distinct existing instance owner/runtime roles required")
	}
	body, e := migrations.Files.ReadFile("001_instance.sql")
	if e != nil {
		return e
	}
	sum := sha256.Sum256(body)
	checksum := hex.EncodeToString(sum[:])
	var present bool
	if e = tx.QueryRow(ctx, "SELECT to_regclass('instance.schema_version') IS NOT NULL").Scan(&present); e != nil {
		return e
	}
	if present {
		var owned bool
		if e = tx.QueryRow(ctx, "SELECT nspowner=(SELECT oid FROM pg_roles WHERE rolname=current_user) FROM pg_namespace WHERE nspname='instance'").Scan(&owned); e != nil {
			return e
		}
		if !owned {
			return fmt.Errorf("instance schema belongs to another owner")
		}
		var old string
		if e = tx.QueryRow(ctx, "SELECT checksum FROM instance.schema_version WHERE version=1").Scan(&old); e != nil {
			return e
		}
		if old != checksum {
			return fmt.Errorf("instance migration checksum differs")
		}
	} else {
		if _, e = tx.Exec(ctx, string(body)); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO instance.schema_version VALUES(1,$1)", checksum); e != nil {
			return e
		}
	}
	for i, name := range instanceSchemaFiles {
		body, e := migrations.Files.ReadFile(name)
		if e != nil {
			return e
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])
		var old string
		e = tx.QueryRow(ctx, "SELECT checksum FROM instance.schema_version WHERE version=$1", i+2).Scan(&old)
		if errors.Is(e, pgx.ErrNoRows) {
			if _, e = tx.Exec(ctx, string(body)); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "INSERT INTO instance.schema_version VALUES($1,$2)", i+2, checksum); e != nil {
				return e
			}
		} else if e != nil {
			return e
		} else if old != checksum {
			return fmt.Errorf("compute preset migration %s checksum differs", name)
		}
	}
	quoted := pgx.Identifier{role}.Sanitize()
	if _, e = tx.Exec(ctx, "GRANT USAGE ON SCHEMA instance TO "+quoted+"; GRANT SELECT ON instance.schema_version TO "+quoted+"; GRANT SELECT,INSERT,UPDATE ON instance.instances,instance.instance_operations,instance.instance_work,instance.workspace_bindings,instance.instance_quota_release_outbox,instance.borrowed_volume_references,instance.network_submissions TO "+quoted); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

const instanceSelect = "SELECT snapshot,version,desired,created_at,updated_at,instance_contract_version FROM instance.instances WHERE tenant_id=$1 AND instance_id=$2"

type scanner interface{ Scan(...any) error }

func scanInstance(row scanner) (biz.Instance, error) {
	var i biz.Instance
	var raw []byte
	var contract int32
	e := row.Scan(&raw, &i.Version, &i.Desired, &i.CreatedAt, &i.UpdatedAt, &contract)
	if errors.Is(e, pgx.ErrNoRows) {
		return i, biz.ErrNotFound
	}
	if e != nil {
		return i, e
	}
	if contract != 2 {
		return i, biz.ErrInvalid
	}
	version, desired, created, updated := i.Version, i.Desired, i.CreatedAt, i.UpdatedAt
	e = decodeSnapshot(raw, &i)
	i.Version = version
	i.Desired = desired
	i.CreatedAt = created
	i.UpdatedAt = updated
	return i, e
}
func (p *Postgres) Get(ctx context.Context, tenant, id string) (biz.Instance, error) {
	r, e := sqlcgen.New(p.pool).GetInstance(ctx, sqlcgen.GetInstanceParams{TenantID: tenant, InstanceID: id})
	if errors.Is(e, pgx.ErrNoRows) {
		return biz.Instance{}, biz.ErrNotFound
	}
	if e != nil {
		return biz.Instance{}, e
	}
	if r.InstanceContractVersion != 2 {
		return biz.Instance{}, biz.ErrInvalid
	}
	return decodeInstance(r.Snapshot, r.Version, r.Desired, r.CreatedAt, r.UpdatedAt)
}
func decodeSnapshot(raw []byte, i *biz.Instance) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if e := dec.Decode(i); e != nil {
		return e
	}
	if dec.Decode(new(any)) != io.EOF {
		return biz.ErrInvalid
	}
	if i.Spec.InstanceKind != biz.KindContainer || (i.Spec.ComputeType != biz.TypeCPU && i.Spec.ComputeType != biz.TypeGPU) {
		return biz.ErrInvalid
	}
	if biz.ValidateConfiguration(i.Spec) != nil || (i.CreateRequestHash != "" && biz.ValidateScheduling(i.Spec.Scheduling) != nil) {
		return biz.ErrInvalid
	}
	if (i.Spec.Network == nil) != (i.Network == nil) {
		return biz.ErrInvalid
	}
	if i.Network != nil {
		n := i.Network
		if !biz.UUID(n.SubmissionID) || n.RequestKey == "" || n.Generation < 1 || n.SnapshotRevision < 1 || (n.State != "open" && n.State != "closing" && n.State != "closed") || n.DesiredRunState != i.Desired || (n.State == "open" && n.FinalizationID != "") || (n.State != "open" && !biz.UUID(n.FinalizationID)) || (n.State == "closed" && (n.ClosedAt == nil || n.ClosedAt.IsZero())) {
			return biz.ErrInvalid
		}
	}
	return nil
}
func decodeInstance(raw []byte, version int64, desired string, created, updated time.Time) (biz.Instance, error) {
	var i biz.Instance
	e := decodeSnapshot(raw, &i)
	i.Version = version
	i.Desired = desired
	i.CreatedAt = created
	i.UpdatedAt = updated
	return i, e
}
func (p *Postgres) List(ctx context.Context, tenant string, f biz.ListFilter) ([]biz.Instance, int64, error) {
	if e := biz.RequireTenant(ctx, tenant); e != nil {
		return nil, 0, e
	}
	_, size, offset, e := biz.PageWindow(f.Page, f.PageSize)
	if e != nil {
		return nil, 0, e
	}
	if (f.InstanceKind != "" && f.InstanceKind != biz.KindContainer) || (f.ComputeType != "" && f.ComputeType != biz.TypeCPU && f.ComputeType != biz.TypeGPU) || (f.ObservedState != "" && f.ObservedState != "pending" && f.ObservedState != "running" && f.ObservedState != "stopped" && f.ObservedState != "deleted") || len([]rune(f.Name)) > 80 {
		return nil, 0, biz.ErrInvalid
	}
	tx, e := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, 0, e
	}
	defer tx.Rollback(ctx)
	where := " WHERE tenant_id=$1 AND ($2::text='' OR snapshot->'Spec'->>'instance_kind'=$2) AND ($3::text='' OR snapshot->'Spec'->>'compute_type'=$3) AND ($4::text='' OR snapshot->>'Observed'=$4) AND ($5::text='' OR position(lower($5) in lower(snapshot->'Spec'->>'Name'))>0)"
	args := []any{tenant, f.InstanceKind, f.ComputeType, f.ObservedState, strings.TrimSpace(f.Name)}
	var total int64
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM instance.instances"+where, args...).Scan(&total); e != nil {
		return nil, 0, e
	}
	args = append(args, int64(size), offset)
	rows, e := tx.Query(ctx, "SELECT snapshot,version,desired,created_at,updated_at,instance_contract_version FROM instance.instances"+where+" ORDER BY created_at DESC,instance_id DESC LIMIT $6 OFFSET $7", args...)
	if e != nil {
		return nil, 0, e
	}
	out := []biz.Instance{}
	for rows.Next() {
		i, e := scanInstance(rows)
		if e != nil {
			rows.Close()
			return nil, 0, e
		}
		out = append(out, i)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, 0, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, 0, e
	}
	return out, total, nil
}
func (p *Postgres) GetOperation(ctx context.Context, tenant, id string) (biz.Operation, error) {
	var o biz.Operation
	var raw []byte
	e := p.pool.QueryRow(ctx, "SELECT tenant_id::text,operation_id::text,instance_id::text,action,idempotency_key,request_hash,version,ack,state,reason,created_at FROM instance.instance_operations WHERE tenant_id=$1 AND operation_id=$2", tenant, id).Scan(&o.TenantID, &o.ID, &o.InstanceID, &o.Action, &o.Key, &o.RequestHash, &o.Version, &raw, &o.State, &o.Reason, &o.CreatedAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return o, biz.ErrNotFound
	}
	if e != nil {
		return o, e
	}
	e = json.Unmarshal(raw, &o.ACK)
	return o, e
}
func (p *Postgres) AcceptCreate(ctx context.Context, c biz.Create) (biz.ACK, error) {
	if e := biz.ValidateCreate(c); e != nil {
		return biz.ACK{}, e
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.ACK{}, e
	}
	defer tx.Rollback(ctx)
	// This serializes admission for one immutable tenant/resource identity, including DELETE-first.
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", c.TenantID+":"+c.ID); e != nil {
		return biz.ACK{}, e
	}
	i, e := scanInstance(tx.QueryRow(ctx, instanceSelect+" FOR UPDATE", c.TenantID, c.ID))
	ack := biz.ACK{OperationID: c.OperationID, ResourceID: c.ID, Accepted: true}
	if e == nil {
		if i.CreateOperationID != c.OperationID {
			return biz.ACK{}, biz.ErrConflict
		}
		if i.CreateRequestHash == "" && i.Desired == biz.Deleted {
			if i.Spec.ComputeType != c.Spec.ComputeType {
				return biz.ACK{}, biz.ErrConflict
			}
			i.CreateRequestHash = c.RequestHash
			i.BusinessDigest = c.BusinessDigest
			i.Spec = c.Spec
			i.VolumeReferencesReleased = true
			i.Network = initialNetworkSubmission(i)
			raw, e := json.Marshal(i)
			if e != nil {
				return ack, e
			}
			if _, e = tx.Exec(ctx, "UPDATE instance.instances SET create_request_hash=$3,snapshot=$4,instance_contract_version=$5 WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID, c.RequestHash, raw, c.Spec.ContractVersion); e != nil {
				return ack, e
			}
			if e = saveNetworkSubmission(ctx, tx, i); e != nil {
				return ack, e
			}
			if e = insertOperation(ctx, tx, i, c.OperationID, "create", c.OperationID, c.RequestHash, ack); e != nil {
				return ack, e
			}
			if _, e = tx.Exec(ctx, "UPDATE instance.instance_operations SET state='superseded' WHERE tenant_id=$1 AND operation_id=$2", i.TenantID, c.OperationID); e != nil {
				return ack, e
			}
			return ack, tx.Commit(ctx)
		}
		if i.Spec.ComputeType != c.Spec.ComputeType || i.CreateRequestHash != c.RequestHash || i.BusinessDigest != c.BusinessDigest {
			return biz.ACK{}, biz.ErrConflict
		}
		var old []byte
		if e = tx.QueryRow(ctx, "SELECT ack FROM instance.instance_operations WHERE tenant_id=$1 AND operation_id=$2 AND request_hash=$3", c.TenantID, c.OperationID, c.RequestHash).Scan(&old); e != nil {
			return biz.ACK{}, e
		}
		e = json.Unmarshal(old, &ack)
		return ack, e
	}
	if !errors.Is(e, biz.ErrNotFound) {
		return biz.ACK{}, e
	}
	i = biz.Instance{TenantID: c.TenantID, ID: c.ID, CreateOperationID: c.OperationID, CreateRequestHash: c.RequestHash, BusinessDigest: c.BusinessDigest, Spec: c.Spec, Version: 1, Desired: biz.Running, Observed: "pending", LastOperationID: c.OperationID, VolumeReferencesReleased: len(c.Spec.VolumeMounts) == 0}
	i.Network = initialNetworkSubmission(i)
	raw, e := json.Marshal(i)
	if e != nil {
		return ack, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO instance.instances(tenant_id,instance_id,create_operation_id,create_request_hash,version,desired,snapshot,instance_contract_version) VALUES($1,$2,$3,$4,1,'running',$5,$6)", c.TenantID, c.ID, c.OperationID, c.RequestHash, raw, c.Spec.ContractVersion); e != nil {
		return ack, e
	}
	if e = saveNetworkSubmission(ctx, tx, i); e != nil {
		return ack, e
	}
	if e = insertOperation(ctx, tx, i, c.OperationID, "create", c.OperationID, c.RequestHash, ack); e != nil {
		return ack, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO instance.workspace_bindings(tenant_id,instance_id,namespace,pvc_name) VALUES($1,$2,$3,$4)", c.TenantID, c.ID, c.Spec.Namespace, c.Spec.PVCName); e != nil {
		return ack, e
	}
	for _, m := range c.Spec.VolumeMounts {
		if _, e = tx.Exec(ctx, "INSERT INTO instance.borrowed_volume_references(tenant_id,instance_id,mount_path,cluster_id,namespace,pvc_name,pvc_uid,read_only) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", c.TenantID, c.ID, m.MountPath, m.ClusterID, m.Namespace, m.PVCName, m.PVCUID, m.ReadOnly); e != nil {
			return ack, e
		}
	}
	if _, e = tx.Exec(ctx, "INSERT INTO instance.instance_work(tenant_id,instance_id) VALUES($1,$2)", c.TenantID, c.ID); e != nil {
		return ack, e
	}
	return ack, tx.Commit(ctx)
}
func (p *Postgres) AcceptDelete(ctx context.Context, d biz.Delete) (biz.ACK, error) {
	ack := biz.ACK{OperationID: d.OperationID, ResourceID: d.ID, Accepted: true}
	if e := biz.ValidateDelete(d); e != nil {
		return ack, e
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return ack, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", d.TenantID+":"+d.ID); e != nil {
		return ack, e
	}
	i, e := scanInstance(tx.QueryRow(ctx, instanceSelect+" FOR UPDATE", d.TenantID, d.ID))
	if errors.Is(e, biz.ErrNotFound) {
		if d.ExpectedVersion > 1 {
			return ack, biz.ErrVersion
		}
		i = biz.Instance{TenantID: d.TenantID, ID: d.ID, CreateOperationID: d.CreateOperationID, Spec: biz.Spec{InstanceKind: d.InstanceKind, ComputeType: d.ComputeType, ContractVersion: 2, WorkspaceMountPath: "/workspace"}, Version: 1, Desired: biz.Deleted, Observed: "pending", LastOperationID: d.OperationID}
		raw, e := json.Marshal(i)
		if e != nil {
			return ack, e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO instance.instances(tenant_id,instance_id,create_operation_id,create_request_hash,version,desired,snapshot,instance_contract_version) VALUES($1,$2,$3,'',1,'deleted',$4,2)", d.TenantID, d.ID, d.CreateOperationID, raw); e != nil {
			return ack, e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO instance.instance_work(tenant_id,instance_id) VALUES($1,$2)", d.TenantID, d.ID); e != nil {
			return ack, e
		}
	} else if e != nil {
		return ack, e
	} else {
		if i.Spec.ComputeType != d.ComputeType || i.CreateOperationID != d.CreateOperationID {
			return ack, biz.ErrConflict
		}
		var old []byte
		var hash string
		e = tx.QueryRow(ctx, "SELECT ack,request_hash FROM instance.instance_operations WHERE tenant_id=$1 AND operation_id=$2", d.TenantID, d.OperationID).Scan(&old, &hash)
		if e == nil {
			if hash != d.RequestHash {
				return ack, biz.ErrConflict
			}
			e = json.Unmarshal(old, &ack)
			return ack, e
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return ack, e
		}
		if d.ExpectedVersion > 0 && i.Version != d.ExpectedVersion {
			return ack, biz.ErrVersion
		}
		i.Version++
		i.Desired = biz.Deleted
		networkIntent(&i)
		i.LastOperationID = d.OperationID
		i.Reason = ""
		raw, e := json.Marshal(i)
		if e != nil {
			return ack, e
		}
		if _, e = tx.Exec(ctx, "UPDATE instance.instances SET desired='deleted',version=$3,snapshot=$4,updated_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID, i.Version, raw); e != nil {
			return ack, e
		}
		if e = saveNetworkSubmission(ctx, tx, i); e != nil {
			return ack, e
		}
		if _, e = tx.Exec(ctx, "UPDATE instance.instance_work SET due_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID); e != nil {
			return ack, e
		}
	}
	if e = insertOperation(ctx, tx, i, d.OperationID, "delete", d.OperationID, d.RequestHash, ack); e != nil {
		return ack, e
	}
	if _, e = tx.Exec(ctx, "UPDATE instance.instance_operations SET state='superseded' WHERE tenant_id=$1 AND instance_id=$2 AND version<$3 AND state='accepted'", i.TenantID, i.ID, i.Version); e != nil {
		return ack, e
	}
	return ack, tx.Commit(ctx)
}
func insertOperation(ctx context.Context, tx pgx.Tx, i biz.Instance, id, action, key, hash string, ack biz.ACK) error {
	raw, e := json.Marshal(ack)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, "INSERT INTO instance.instance_operations(tenant_id,operation_id,instance_id,action,idempotency_key,request_hash,version,ack) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", i.TenantID, id, i.ID, action, key, hash, i.Version, raw)
	return e
}
func (p *Postgres) AcceptCommand(ctx context.Context, c biz.Command) (biz.ACK, error) {
	if !biz.UUID(c.TenantID) || !biz.UUID(c.ID) || !biz.UUID(c.OperationID) || !biz.UUID(c.Key) || len(c.RequestHash) != 64 || c.ExpectedVersion < 1 {
		return biz.ACK{}, biz.ErrInvalid
	}
	desired := map[string]string{"start": biz.Running, "stop": biz.Stopped, "delete": biz.Deleted}[c.Action]
	if desired == "" {
		return biz.ACK{}, biz.ErrInvalid
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.ACK{}, e
	}
	defer tx.Rollback(ctx)
	i, e := scanInstance(tx.QueryRow(ctx, instanceSelect+" FOR UPDATE", c.TenantID, c.ID))
	if e != nil {
		return biz.ACK{}, e
	}
	var raw []byte
	var hash string
	e = tx.QueryRow(ctx, "SELECT ack,request_hash FROM instance.instance_operations WHERE tenant_id=$1 AND instance_id=$2 AND action=$3 AND idempotency_key=$4", c.TenantID, c.ID, c.Action, c.Key).Scan(&raw, &hash)
	if e == nil {
		if hash != c.RequestHash {
			return biz.ACK{}, biz.ErrConflict
		}
		var ack biz.ACK
		e = json.Unmarshal(raw, &ack)
		return ack, e
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return biz.ACK{}, e
	}
	if i.Version != c.ExpectedVersion || i.Desired == biz.Deleted {
		return biz.ACK{}, biz.ErrVersion
	}
	i.Version++
	i.Desired = desired
	networkIntent(&i)
	i.LastOperationID = c.OperationID
	i.Reason = ""
	raw, e = json.Marshal(i)
	if e != nil {
		return biz.ACK{}, e
	}
	ack := biz.ACK{OperationID: c.OperationID, ResourceID: c.ID, Accepted: true}
	if _, e = tx.Exec(ctx, "UPDATE instance.instances SET version=$3,desired=$4,snapshot=$5,updated_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2", c.TenantID, c.ID, i.Version, i.Desired, raw); e != nil {
		return ack, e
	}
	if e = saveNetworkSubmission(ctx, tx, i); e != nil {
		return ack, e
	}
	if e = insertOperation(ctx, tx, i, c.OperationID, c.Action, c.Key, c.RequestHash, ack); e != nil {
		return ack, e
	}
	if _, e = tx.Exec(ctx, "UPDATE instance.instance_operations SET state='superseded' WHERE tenant_id=$1 AND instance_id=$2 AND version<$3 AND state='accepted'", c.TenantID, c.ID, i.Version); e != nil {
		return ack, e
	}
	if _, e = tx.Exec(ctx, "UPDATE instance.instance_work SET due_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2", c.TenantID, c.ID); e != nil {
		return ack, e
	}
	return ack, tx.Commit(ctx)
}
func (p *Postgres) Claim(ctx context.Context, owner string, lease time.Duration) (*biz.Work, error) {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	var tenant, id string
	var epoch int64
	var until time.Time
	e = tx.QueryRow(ctx, "WITH candidate AS (SELECT tenant_id,instance_id FROM instance.instance_work WHERE due_at<=clock_timestamp() AND lease_until<clock_timestamp() ORDER BY due_at FOR UPDATE SKIP LOCKED LIMIT 1) UPDATE instance.instance_work w SET lease_owner=$1,lease_epoch=w.lease_epoch+1,lease_until=clock_timestamp()+($2::bigint*interval '1 millisecond') FROM candidate c WHERE w.tenant_id=c.tenant_id AND w.instance_id=c.instance_id RETURNING w.tenant_id::text,w.instance_id::text,w.lease_epoch,w.lease_until", owner, lease.Milliseconds()).Scan(&tenant, &id, &epoch, &until)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	i, e := scanInstance(tx.QueryRow(ctx, instanceSelect, tenant, id))
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return &biz.Work{Instance: i, Owner: owner, Epoch: epoch, LeaseUntil: until}, nil
}
func lockWork(ctx context.Context, tx pgx.Tx, w biz.Work) error {
	var valid bool
	e := tx.QueryRow(ctx, "SELECT lease_owner=$3 AND lease_epoch=$4 AND lease_until>clock_timestamp() FROM instance.instance_work WHERE tenant_id=$1 AND instance_id=$2 FOR UPDATE", w.Instance.TenantID, w.Instance.ID, w.Owner, w.Epoch).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return biz.ErrLease
	}
	return nil
}
func (p *Postgres) BeginMutation(ctx context.Context, w biz.Work, m biz.Mutation) error {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	// Admission and worker consistently lock the instance before its work row.
	i, e := scanInstance(tx.QueryRow(ctx, instanceSelect+" FOR UPDATE", w.Instance.TenantID, w.Instance.ID))
	if e != nil {
		return e
	}
	if e = lockWork(ctx, tx, w); e != nil {
		return e
	}
	if i.Version != m.Version || i.LastOperationID != m.OperationID || i.Pending != nil {
		return biz.ErrVersion
	}
	i.Pending = &m
	i.GPUPods = biz.MergePodRefs(i.GPUPods, w.Instance.GPUPods)
	if m.Kind == "create_notebook" || w.Instance.ExternalComputeStarted {
		i.ExternalComputeStarted = true
	}
	raw, e := json.Marshal(i)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE instance.instances SET snapshot=$3 WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID, raw); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (p *Postgres) Finish(ctx context.Context, w biz.Work, progress biz.Progress) error {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	i, e := scanInstance(tx.QueryRow(ctx, instanceSelect+" FOR UPDATE", w.Instance.TenantID, w.Instance.ID))
	if e != nil {
		return e
	}
	if e = lockWork(ctx, tx, w); e != nil {
		return e
	}
	if receipt := progress.Receipt; receipt != nil {
		m := receipt.Mutation
		if i.Pending == nil || *i.Pending != m {
			return biz.ErrVersion
		}
		raw, e := json.Marshal(receipt)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "UPDATE instance.instance_operations SET external_result=$3 WHERE tenant_id=$1 AND operation_id=$2", i.TenantID, m.OperationID, raw); e != nil {
			return e
		}
	}
	if i.Version == w.Instance.Version {
		progress.Instance.GPUPods = biz.MergePodRefs(i.GPUPods, progress.Instance.GPUPods)
		progress.Instance.ExternalComputeStarted = i.ExternalComputeStarted || progress.Instance.ExternalComputeStarted
		// An outbox ACK may commit while this worker owns its lease. Keep that
		// monotone result when saving an otherwise unchanged business version.
		progress.Instance.QuotaReturned = i.QuotaReturned || progress.Instance.QuotaReturned
		if !i.CleanupCompletedAt.IsZero() {
			progress.Instance.CleanupCompletedAt = i.CleanupCompletedAt
		}
		i = progress.Instance
		if e = saveNetworkSubmission(ctx, tx, i); e != nil {
			return e
		}
		if i.Desired == biz.Deleted && i.ComputeCleaned {
			if _, e = tx.Exec(ctx, "UPDATE instance.borrowed_volume_references SET state='released' WHERE tenant_id=$1 AND instance_id=$2 AND state='active'", i.TenantID, i.ID); e != nil {
				return e
			}
			i.VolumeReferencesReleased = true
		}
		if progress.CleanupComplete {
			if e = sealQuotaRelease(ctx, tx, &i); e != nil {
				return e
			}
		}
		if biz.OwnerCleanupReady(i) && i.QuotaReturned {
			i.Reason = ""
		}
		raw, e := json.Marshal(i)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "UPDATE instance.instances SET snapshot=$3,updated_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID, raw); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "UPDATE instance.workspace_bindings SET pvc_uid=$3,cleanup_state=$4 WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID, i.PVCUID, map[bool]string{true: "cleaned", false: "pending"}[i.WorkspaceCleaned]); e != nil {
			return e
		}
		state := "accepted"
		if i.Reason == "" && i.Pending == nil && ((i.Desired == biz.Deleted && i.ComputeCleaned && i.WorkspaceCleaned && i.QuotaReturned && (i.Network == nil || i.Network.Cleaned)) || (i.Desired != biz.Deleted && i.Desired == i.Observed)) {
			state = "completed"
		}
		if i.Reason == "CREATE_RESULT_UNKNOWN" || i.Reason == "WORKSPACE_CREATE_RESULT_UNKNOWN" {
			state = "blocked"
		}
		if _, e = tx.Exec(ctx, "UPDATE instance.instance_operations SET state=$3,reason=$4 WHERE tenant_id=$1 AND operation_id=$2", i.TenantID, i.LastOperationID, state, i.Reason); e != nil {
			return e
		}
	} else {
		// A newer intent must not discard an attributable old call result. Keep
		// the current business version and retain only immutable runtime evidence.
		i.GPUPods = biz.MergePodRefs(i.GPUPods, progress.Instance.GPUPods)
		i.ExternalComputeStarted = i.ExternalComputeStarted || progress.Instance.ExternalComputeStarted
		mergeNetworkResult(i.Network, progress.Instance.Network)
		if e = saveNetworkSubmission(ctx, tx, i); e != nil {
			return e
		}
		if progress.Receipt != nil {
			i.Pending = nil
			if i.NotebookUID == "" {
				i.NotebookUID = progress.Instance.NotebookUID
			}
			if i.PVCUID == "" {
				i.PVCUID = progress.Instance.PVCUID
			}
			if i.StatefulSetUID == "" {
				i.StatefulSetUID = progress.Instance.StatefulSetUID
			}
			if i.VolumeName == "" {
				i.VolumeName = progress.Instance.VolumeName
				i.VolumeUID = progress.Instance.VolumeUID
			}
		}
		raw, e := json.Marshal(i)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "UPDATE instance.instances SET snapshot=$3,updated_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID, raw); e != nil {
			return e
		}
		progress.Next = time.Millisecond
	}
	delay := progress.Next.Milliseconds()
	if progress.Next == 0 {
		delay = int64((365 * 24 * time.Hour) / time.Millisecond)
	}
	if _, e = tx.Exec(ctx, "UPDATE instance.instance_work SET lease_until='-infinity',lease_owner='',due_at=clock_timestamp()+($3::bigint*interval '1 millisecond') WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID, delay); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

var _ biz.Repository = (*Postgres)(nil)
