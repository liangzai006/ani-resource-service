//go:build imageintegration

package data_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	imagedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image/sqlcgen"
)

type fixture struct {
	Owner, Runtime                               *pgxpool.Pool
	Repo                                         *imagedata.Postgres
	OwnerDSN, RuntimeDSN, OwnerRole, RuntimeRole string
}

func database(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	path := os.Getenv("IMAGE_TEST_ADMIN_DSN_FILE")
	if path == "" {
		t.Fatal("imageintegration requires scripts/image-integration; missing DSN file is not a skip")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("test DSN file unavailable")
	}
	address, err := url.Parse(string(body))
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	admin, err := pgxpool.New(ctx, string(body))
	if err != nil {
		t.Fatal("test admin pool unavailable")
	}
	t.Cleanup(admin.Close)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	dbName := "image_test_" + suffix
	ownerRole := "image_owner_" + suffix
	runtimeRole := "image_runtime_" + suffix
	password, _ := address.User.Password()
	literal := "'" + strings.ReplaceAll(password, "'", "''") + "'"
	for _, role := range []string{ownerRole, runtimeRole} {
		if _, err = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD "+literal); err != nil {
			t.Fatal("test role creation failed")
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error("test role cleanup failed")
			}
		})
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()+" OWNER "+pgx.Identifier{ownerRole}.Sanitize()); err != nil {
		t.Fatal("test database creation failed")
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error("test database cleanup failed")
		}
	})
	address.Path = "/" + dbName
	address.User = url.UserPassword(ownerRole, password)
	ownerDSN := address.String()
	owner, err := pgxpool.New(ctx, ownerDSN)
	if err != nil {
		t.Fatal("test owner pool unavailable")
	}
	t.Cleanup(owner.Close)
	// Deployment preparation in this exclusive fixture, not Image migration.
	if _, err = owner.Exec(ctx, "REVOKE CREATE,TEMPORARY ON DATABASE "+pgx.Identifier{dbName}.Sanitize()+" FROM PUBLIC; REVOKE CREATE ON SCHEMA public FROM PUBLIC; CREATE TABLE public.network_image_guard(id integer)"); err != nil {
		t.Fatal("test privilege preparation failed")
	}
	cfg := imagedata.OwnerConfig{DSN: ownerDSN, RuntimeRole: runtimeRole}
	if err = imagedata.ApplyImageMigrations(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err = imagedata.ApplyImageMigrations(ctx, cfg); err != nil {
		t.Fatal("migration replay:", err)
	}
	address.User = url.UserPassword(runtimeRole, password)
	runtimeDSN := address.String()
	runtime, err := pgxpool.New(ctx, runtimeDSN)
	if err != nil {
		t.Fatal("test runtime pool unavailable")
	}
	t.Cleanup(runtime.Close)
	repo, err := imagedata.OpenPostgres(ctx, runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	return &fixture{Owner: owner, Runtime: runtime, Repo: repo, OwnerDSN: ownerDSN, RuntimeDSN: runtimeDSN, OwnerRole: ownerRole, RuntimeRole: runtimeRole}
}

func command(tenant, space, kind string) biz.Command {
	id := uuid.NewString()
	h := sha256.Sum256([]byte(id))
	return biz.Command{ID: id, TenantID: tenant, SpaceID: space, Scope: biz.TenantImages, Key: id, Kind: kind, Actor: "user:test", Fingerprint: hex.EncodeToString(h[:])}
}
func reserve(t *testing.T, f *fixture, tenant, project string) biz.Space {
	t.Helper()
	s := biz.Space{ID: uuid.NewString(), TenantID: tenant, Scope: biz.TenantImages, InstallationID: uuid.NewString(), RegistryAuthority: "registry.invalid", ProjectName: project}
	s, c, err := f.Repo.ReserveTenantSpace(context.Background(), s, command(tenant, s.ID, "enable_space"))
	if err != nil {
		t.Fatal(err)
	}
	c.Result = biz.CommandResult{Space: &s}
	if _, err = f.Repo.CompleteTenantCommand(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return s
}
func registration(s biz.Space, name, digit string) biz.Registration {
	return biz.Registration{ID: "img_" + strings.ReplaceAll(uuid.NewString(), "-", ""), TenantID: s.TenantID, SpaceID: s.ID, Scope: biz.TenantImages, Repository: "repo", SourceReference: "registry.invalid/" + s.ProjectName + "/repo:v1", Digest: "sha256:" + strings.Repeat(digit, 64), MediaType: "application/vnd.oci.image.manifest.v1+json", Platforms: []biz.ImagePlatform{{OS: "linux", Architecture: "amd64"}}, Metadata: biz.Metadata{DisplayName: name, Purposes: []string{"container"}, Accelerator: "none"}}
}

func TestTenantQueriesAndAtomicCatalog(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	a, b := uuid.NewString(), uuid.NewString()
	sa, sb := reserve(t, f, a, "t-alpha"), reserve(t, f, b, "t-beta")
	input := registration(sa, "alpha", "a")
	cmd := command(a, sa.ID, "register_image")
	first, err := f.Repo.ApplyTenantRegistration(ctx, cmd, input)
	if err != nil {
		t.Fatal(err)
	}
	// Same request replays its original snapshot, regardless of a later candidate.
	input.ID = "img_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	input.Digest = "sha256:" + strings.Repeat("b", 64)
	replay, err := f.Repo.ApplyTenantRegistration(ctx, cmd, input)
	if err != nil || replay.ID != first.ID || replay.Digest != first.Digest || !replay.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("registration replay changed")
	}
	otherActor := cmd
	otherActor.Actor = "user:other"
	if _, err = f.Repo.ApplyTenantRegistration(ctx, otherActor, input); biz.ReasonOf(err) != biz.IdempotencyConflict {
		t.Fatal("actor replay boundary", err)
	}
	qb := sqlcgen.New(f.Runtime)
	if _, err = qb.GetTenantRegistration(ctx, sqlcgen.GetTenantRegistrationParams{TenantID: &b, ImageID: first.ID}); err != pgx.ErrNoRows {
		t.Fatal("tenant query exposed another tenant image")
	}
	if _, err = f.Repo.FindTenantRegistration(ctx, b, first.ID); biz.ReasonOf(err) != biz.ImageNotFound {
		t.Fatal("cross tenant get", err)
	}
	if _, err = f.Repo.FindTenantCommand(ctx, b, sa.ID, cmd.Key); biz.ReasonOf(err) != biz.ImageNotFound {
		t.Fatal("cross tenant command", err)
	}
	if _, err = f.Repo.ApplyTenantMetadata(ctx, command(b, sb.ID, "update_image"), biz.UpdateImage{TenantID: b, ImageID: first.ID, ExpectedVersion: 1, Metadata: first.Metadata}); biz.ReasonOf(err) != biz.ImageNotFound {
		t.Fatal("cross tenant update", err)
	}
	if _, err = f.Repo.ApplyTenantUnregister(ctx, command(b, sb.ID, "unregister_image"), biz.UnregisterImage{TenantID: b, ImageID: first.ID, ExpectedVersion: 1}); biz.ReasonOf(err) != biz.ImageNotFound {
		t.Fatal("cross tenant unregister", err)
	}
	if _, err = f.Repo.GetTenantPull(ctx, b, sa.ID); biz.ReasonOf(err) != biz.SpaceNotFound {
		t.Fatal("cross tenant pull", err)
	}
	if _, err = f.Repo.GetTenantPublisher(ctx, b, sa.ID); biz.ReasonOf(err) != biz.SpaceNotFound {
		t.Fatal("cross tenant publisher", err)
	}
	// Fail the final command write: registration and command must both roll back.
	if _, err = f.Owner.Exec(ctx, `CREATE FUNCTION image.reject_completion() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected'; END$$; CREATE TRIGGER reject_completion BEFORE UPDATE ON image.commands FOR EACH ROW EXECUTE FUNCTION image.reject_completion()`); err != nil {
		t.Fatal(err)
	}
	rollback := registration(sa, "rollback", "c")
	rollbackCommand := command(a, sa.ID, "register_image")
	if _, err = f.Repo.ApplyTenantRegistration(ctx, rollbackCommand, rollback); err == nil {
		t.Fatal("injected commit failure accepted")
	}
	if _, err = f.Repo.FindTenantRegistration(ctx, a, rollback.ID); biz.ReasonOf(err) != biz.ImageNotFound {
		t.Fatal("partial registration survived", err)
	}
	if _, err = f.Repo.FindTenantCommand(ctx, a, sa.ID, rollbackCommand.Key); biz.ReasonOf(err) != biz.ImageNotFound {
		t.Fatal("partial command survived", err)
	}
}

func TestCatalogCASAndPagination(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	a := uuid.NewString()
	s := reserve(t, f, a, "t-page")
	var target biz.Registration
	for i, v := range []struct{ name, purpose, digit string }{{"match%one", "training", "a"}, {"skip", "container", "b"}, {"match%two", "training", "c"}, {"skip", "container", "d"}, {"match%three", "training", "e"}} {
		r := registration(s, v.name, v.digit)
		r.Metadata.Purposes = []string{v.purpose}
		var err error
		r, err = f.Repo.ApplyTenantRegistration(ctx, command(a, s.ID, "register_image"), r)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			target = r
		}
	}
	filter := biz.Filter{Search: "match%", Purposes: []string{"training"}, Limit: 1}
	first, _, err := f.Repo.PageTenantRegistrations(ctx, a, filter, nil)
	if err != nil || len(first) != 2 {
		t.Fatal("filters must apply before limit+1", len(first), err)
	}
	next, _, err := f.Repo.PageTenantRegistrations(ctx, a, filter, &biz.PageKey{CreatedAt: first[0].CreatedAt, ImageID: first[0].ID})
	if err != nil || len(next) != 2 || next[0].ID == first[0].ID {
		t.Fatal("page continuation failed", err)
	}
	empty, _, err := f.Repo.PageTenantRegistrations(ctx, uuid.NewString(), filter, nil)
	if err != nil || len(empty) != 0 {
		t.Fatal("cross tenant page", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, name := range []string{"new-one", "new-two"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			m := target.Metadata
			m.DisplayName = name
			_, err := f.Repo.ApplyTenantMetadata(ctx, command(a, s.ID, "update_image"), biz.UpdateImage{TenantID: a, ImageID: target.ID, ExpectedVersion: 1, Metadata: m})
			results <- err
		}(name)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if biz.ReasonOf(err) == biz.VersionConflict {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("CAS did not serialize", success, conflict)
	}
	current, err := f.Repo.FindTenantRegistration(ctx, a, target.ID)
	if err != nil || current.Digest != target.Digest || current.Version != 2 {
		t.Fatal("metadata rewrote immutable image", err)
	}
	tomb, err := f.Repo.ApplyTenantUnregister(ctx, command(a, s.ID, "unregister_image"), biz.UnregisterImage{TenantID: a, ImageID: target.ID, ExpectedVersion: 2})
	if err != nil || tomb.UnregisteredAt == nil {
		t.Fatal(err)
	}
	replacement := registration(s, "new registration", "a")
	if r, err := f.Repo.ApplyTenantRegistration(ctx, command(a, s.ID, "register_image"), replacement); err != nil || r.ID == target.ID {
		t.Fatal("cancelled registration incorrectly revived", err)
	}
}

func TestReferenceLookupIsExactScopedAndIndependentOfCatalogSize(t *testing.T) {
	f, _, _, _, ctx := lifecycleFixture(t)
	a, b := uuid.NewString(), uuid.NewString()
	sa, sb := reserve(t, f, a, "t-alpha"), reserve(t, f, b, "t-beta")
	platform, err := f.Repo.FindPlatformSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// More than 100 registrations share the same repository. The exact tag
	// query must return the requested row, not fail because of other tags.
	_, err = f.Owner.Exec(ctx, `
INSERT INTO image.registrations
 (image_id,space_id,owner_scope,tenant_id,display_name,repository,source_reference,digest,media_type,platforms,purposes,accelerator,created_by,updated_by)
SELECT 'img_' || md5(i::text), $1, 'tenant', $2, 'image', 'repo',
 'registry.invalid/t-alpha/repo:v' || i, 'sha256:' || lpad(to_hex(i),64,'0'),
 'application/vnd.oci.image.manifest.v1+json', '[{"os":"linux","architecture":"amd64"}]'::jsonb,
 ARRAY['container'], 'none', 'test', 'test' FROM generate_series(1,120) i`, sa.ID, a)
	if err != nil {
		t.Fatal(err)
	}
	ref := biz.ReferenceLookup{SpaceID: sa.ID, Repository: "repo", Reference: "registry.invalid/t-alpha/repo:v120"}
	rows, err := f.Repo.FindTenantRegistrationsByReference(ctx, a, ref)
	if err != nil || len(rows) != 1 || rows[0].SourceReference != ref.Reference {
		t.Fatalf("exact tag = %#v, %v", rows, err)
	}
	ref.IsDigest, ref.Reference = true, rows[0].Digest
	byDigest, err := f.Repo.FindTenantRegistrationsByReference(ctx, a, ref)
	if err != nil || len(byDigest) != 1 || byDigest[0].ID != rows[0].ID {
		t.Fatalf("exact digest = %#v, %v", byDigest, err)
	}
	other, err := f.Repo.FindTenantRegistrationsByReference(ctx, b, ref)
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-tenant lookup = %#v, %v", other, err)
	}
	ref.SpaceID = sb.ID
	other, err = f.Repo.FindTenantRegistrationsByReference(ctx, a, ref)
	if err != nil || len(other) != 0 {
		t.Fatalf("wrong space lookup = %#v, %v", other, err)
	}
	ref.SpaceID = platform.ID
	other, err = f.Repo.FindPlatformRegistrationsByReference(ctx, ref)
	if err != nil || len(other) != 0 {
		t.Fatalf("tenant digest in platform space = %#v, %v", other, err)
	}
	_, err = f.Owner.Exec(ctx, `INSERT INTO image.registrations
 (image_id,space_id,owner_scope,tenant_id,display_name,repository,source_reference,digest,media_type,platforms,purposes,accelerator,created_by,updated_by)
 VALUES ('img_' || md5('platform'), $1, 'platform', NULL, 'image', 'repo', 'registry.invalid/platform/repo:v120', $2,
 'application/vnd.oci.image.manifest.v1+json', '[{"os":"linux","architecture":"amd64"}]'::jsonb,
 ARRAY['container'], 'none', 'test', 'test')`, platform.ID, ref.Reference)
	if err != nil {
		t.Fatal(err)
	}
	other, err = f.Repo.FindPlatformRegistrationsByReference(ctx, ref)
	if err != nil || len(other) != 1 || other[0].Scope != biz.PlatformImages {
		t.Fatalf("platform digest = %#v, %v", other, err)
	}
	// Two active registrations with the same tag and different digests are
	// ambiguous. The bounded query must return enough rows to detect it.
	_, err = f.Owner.Exec(ctx, `INSERT INTO image.registrations
 (image_id,space_id,owner_scope,tenant_id,display_name,repository,source_reference,digest,media_type,platforms,purposes,accelerator,created_by,updated_by)
 VALUES ('img_' || md5('conflict'), $1, 'tenant', $2, 'image', 'repo', 'registry.invalid/t-alpha/repo:v120', 'sha256:' || repeat('f',64),
 'application/vnd.oci.image.manifest.v1+json', '[{"os":"linux","architecture":"amd64"}]'::jsonb,
 ARRAY['container'], 'none', 'test', 'test')`, sa.ID, a)
	if err != nil {
		t.Fatal(err)
	}
	ref = biz.ReferenceLookup{SpaceID: sa.ID, Repository: "repo", Reference: "registry.invalid/t-alpha/repo:v120"}
	rows, err = f.Repo.FindTenantRegistrationsByReference(ctx, a, ref)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ambiguous tag = %#v, %v", rows, err)
	}
	if _, err = f.Owner.Exec(ctx, `UPDATE image.registrations SET unregistered_at=now() WHERE image_id='img_' || md5('conflict')`); err != nil {
		t.Fatal(err)
	}
	rows, err = f.Repo.FindTenantRegistrationsByReference(ctx, a, ref)
	if err != nil || len(rows) != 1 {
		t.Fatalf("unregistered row visible = %#v, %v", rows, err)
	}
}

func TestConcurrentSpaceSlug(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant := uuid.NewString()
			s := biz.Space{ID: uuid.NewString(), TenantID: tenant, Scope: biz.TenantImages, InstallationID: uuid.NewString(), RegistryAuthority: "registry.invalid", ProjectName: "t-race"}
			_, _, err := f.Repo.ReserveTenantSpace(ctx, s, command(tenant, s.ID, "enable_space"))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if biz.ReasonOf(err) == biz.SpaceNameConflict {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("slug race", success, conflict)
	}
}

func TestRuntimeRoleAndMigration(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	for _, statement := range []string{"CREATE TABLE image.bad(id integer)", "CREATE TABLE public.bad(id integer)", "CREATE TEMP TABLE bad(id integer)", "SET ROLE " + pgx.Identifier{f.OwnerRole}.Sanitize(), "SELECT * FROM public.network_image_guard", "UPDATE image.schema_version SET checksum=repeat('0',64)", "DELETE FROM image.spaces"} {
		if _, err := f.Runtime.Exec(ctx, statement); err == nil {
			t.Fatalf("runtime accepted forbidden statement: %s", statement)
		}
	}
	var before string
	if err := f.Owner.QueryRow(ctx, "SELECT checksum FROM image.schema_version WHERE version=1").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Owner.Exec(ctx, "UPDATE image.schema_version SET checksum=repeat('0',64) WHERE version=1"); err != nil {
		t.Fatal(err)
	}
	if err := f.Repo.CheckReady(ctx); err == nil {
		t.Fatal("checksum drift accepted by readiness")
	}
	if err := imagedata.ApplyImageMigrations(ctx, imagedata.OwnerConfig{DSN: f.OwnerDSN, RuntimeRole: f.RuntimeRole}); err == nil {
		t.Fatal("checksum drift accepted by migration")
	}
	if _, err := f.Owner.Exec(ctx, "UPDATE image.schema_version SET checksum=$1 WHERE version=1", before); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Owner.Exec(ctx, "ALTER TABLE image.spaces ENABLE ROW LEVEL SECURITY"); err != nil {
		t.Fatal(err)
	}
	if err := f.Repo.CheckReady(ctx); err == nil {
		t.Fatal("RLS accepted")
	}
	if _, err := f.Owner.Exec(ctx, "ALTER TABLE image.spaces DISABLE ROW LEVEL SECURITY; GRANT SELECT ON public.network_image_guard TO PUBLIC"); err != nil {
		t.Fatal(err)
	}
	if err := f.Repo.CheckReady(ctx); err == nil {
		t.Fatal("inherited cross-domain access accepted")
	}
	if _, err := f.Owner.Exec(ctx, "REVOKE SELECT ON public.network_image_guard FROM PUBLIC"); err != nil {
		t.Fatal(err)
	}
	if err := f.Repo.CheckReady(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTenantCompositeFKAndPlatformFK(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	a, b, space, platform := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := f.Runtime.Exec(ctx, `INSERT INTO image.spaces(space_id,owner_scope,tenant_id,installation_id,registry_authority,project_name) VALUES($1,'tenant',$2,$3,'registry.invalid','t-team')`, space, a, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Runtime.Exec(ctx, `INSERT INTO image.spaces(space_id,owner_scope,installation_id,registry_authority,project_name) VALUES($1,'platform',$2,'registry.invalid','platform')`, platform, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	// Each child relation must reject wrong tenant and scope using a runtime role.
	for _, relation := range []string{"credentials", "commands", "registrations"} {
		for _, v := range []struct {
			scope  string
			tenant any
			space  string
			valid  bool
		}{{"tenant", b, space, false}, {"platform", nil, space, false}, {"tenant", a, platform, false}, {"tenant", nil, space, false}, {"tenant", a, space, true}, {"platform", nil, platform, true}} {
			var err error
			switch relation {
			case "credentials":
				_, err = f.Runtime.Exec(ctx, `INSERT INTO image.credentials(space_id,owner_scope,tenant_id,purpose,state) VALUES($1,$2,$3,'publisher','issuing')`, v.space, v.scope, v.tenant)
			case "commands":
				_, err = f.Runtime.Exec(ctx, `INSERT INTO image.commands(command_id,space_id,owner_scope,tenant_id,idempotency_key,kind,actor,fingerprint,state) VALUES($1,$2,$3,$4,$5,'register_image','test',repeat('a',64),'pending')`, uuid.NewString(), v.space, v.scope, v.tenant, uuid.NewString())
			case "registrations":
				_, err = f.Runtime.Exec(ctx, `INSERT INTO image.registrations(image_id,space_id,owner_scope,tenant_id,display_name,repository,source_reference,digest,media_type,platforms,purposes,created_by,updated_by) VALUES($1,$2,$3,$4,'test','repo','registry.invalid/t-team/repo:v1','sha256:'||repeat('a',64),'application/vnd.oci.image.manifest.v1+json','[{"OS":"linux","Architecture":"amd64"}]',ARRAY['container'],'test','test')`, "img_"+strings.ReplaceAll(uuid.NewString(), "-", ""), v.space, v.scope, v.tenant)
			}
			if v.valid && err != nil {
				t.Fatalf("valid %s relation rejected: %v", relation, err)
			}
			if !v.valid && err == nil {
				t.Fatalf("invalid %s relation accepted: scope %s space %s", relation, v.scope, v.space)
			}
		}
	}
}

func TestCredentialGenerationAndSpaceReadiness(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	s := reserve(t, f, uuid.NewString(), "t-credential")
	q := sqlcgen.New(f.Runtime)
	bound, err := f.Repo.BindTenantProject(ctx, s, 42)
	if err != nil {
		t.Fatal(err)
	}
	available := sqlcgen.SetTenantSpaceAvailableParams{TenantID: &s.TenantID, SpaceID: s.ID, ExpectedVersion: bound.Version}
	if _, err = q.SetTenantSpaceAvailable(ctx, available); err != pgx.ErrNoRows {
		t.Fatal("project alone made space available", err)
	}
	issued, err := q.PutTenantCandidateState(ctx, sqlcgen.PutTenantCandidateStateParams{TenantID: &s.TenantID, SpaceID: s.ID, Purpose: "pull"})
	if err != nil {
		t.Fatal(err)
	}
	name, key := "restricted-pull", "key-v1"
	expires := time.Now().UTC().Add(time.Hour)
	active := sqlcgen.ActivateTenantCredentialParams{TenantID: &s.TenantID, SpaceID: s.ID, Purpose: "pull", ExpectedVersion: issued.Version, Generation: 1, RobotID: pgtype.Int8{Int64: 100, Valid: true}, RobotName: &name, Username: &name, ExpiresAt: &expires, SecretKeyID: &key, SecretCiphertext: []byte("opaque-encrypted-envelope")}
	current, err := q.ActivateTenantCredential(ctx, active)
	if err != nil {
		t.Fatal(err)
	}
	active.Generation = 2
	if _, err = q.ActivateTenantCredential(ctx, active); err != pgx.ErrNoRows {
		t.Fatal("stale credential version accepted", err)
	}
	active.ExpectedVersion = current.Version
	active.Generation = 1
	if _, err = q.ActivateTenantCredential(ctx, active); err != pgx.ErrNoRows {
		t.Fatal("credential generation reused", err)
	}
	if _, err = q.SetTenantSpaceAvailable(ctx, available); err != nil {
		t.Fatal("ready pull did not enable space", err)
	}
	other := uuid.NewString()
	if _, err = q.GetTenantPullCredential(ctx, sqlcgen.GetTenantPullCredentialParams{TenantID: &other, SpaceID: s.ID}); err != pgx.ErrNoRows {
		t.Fatal("cross-tenant pull SQL exposed row", err)
	}
	publisher, err := f.Repo.GetTenantPublisher(ctx, s.TenantID, s.ID)
	if err != nil || publisher.State != "not_issued" || publisher.Version != 0 {
		t.Fatal("metadata read issued a publisher", err)
	}
}

func TestSpaceWriteLockTimeoutAndRelease(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	space := uuid.NewString()
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- f.Repo.WithSpaceWriteLock(ctx, space, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-result:
		t.Fatal("first writer did not acquire lock", err)
	}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	err := f.Repo.WithSpaceWriteLock(short, space, func(context.Context) error {
		t.Error("concurrent space writer entered")
		return nil
	})
	close(release)
	if first := <-result; first != nil {
		t.Fatal(first)
	}
	if biz.ReasonOf(err) != biz.RequestInProgress {
		t.Fatal("lock timeout not bounded", err)
	}
	if err = f.Repo.WithSpaceWriteLock(ctx, space, func(context.Context) error { return nil }); err != nil {
		t.Fatal("lock connection remained poisoned", err)
	}
}
