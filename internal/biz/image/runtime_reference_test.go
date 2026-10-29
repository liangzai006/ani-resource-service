package biz

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

const referenceTenant = "11111111-1111-4111-8111-111111111111"

type referenceCatalog struct {
	CatalogRepository
	tenant, platform []Registration
}

func (c *referenceCatalog) FindTenantRegistrationsByReference(_ context.Context, tenant string, lookup ReferenceLookup) ([]Registration, error) {
	if tenant != referenceTenant {
		panic("unexpected tenant")
	}
	return c.find(c.tenant, lookup), nil
}
func (c *referenceCatalog) FindPlatformRegistrationsByReference(_ context.Context, lookup ReferenceLookup) ([]Registration, error) {
	return c.find(c.platform, lookup), nil
}
func (c *referenceCatalog) find(rows []Registration, lookup ReferenceLookup) []Registration {
	var found []Registration
	for _, row := range rows {
		if row.SpaceID != lookup.SpaceID || row.Repository != lookup.Repository || row.UnregisteredAt != nil {
			continue
		}
		if (lookup.IsDigest && row.Digest == lookup.Reference) || (!lookup.IsDigest && row.SourceReference == lookup.Reference) {
			found = append(found, row)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ID < found[j].ID })
	if len(found) > 2 {
		found = found[:2]
	}
	return found
}
func (c *referenceCatalog) FindTenantRegistration(_ context.Context, tenant, id string) (Registration, error) {
	if tenant == referenceTenant {
		for _, row := range c.tenant {
			if row.ID == id {
				return row, nil
			}
		}
	}
	return Registration{}, Fail(ImageNotFound, "not found")
}
func (c *referenceCatalog) FindPlatformRegistration(_ context.Context, id string) (Registration, error) {
	for _, row := range c.platform {
		if row.ID == id {
			return row, nil
		}
	}
	return Registration{}, Fail(ImageNotFound, "not found")
}

type referenceSpaces struct {
	SpaceRepository
	tenant, platform Space
}

func (s referenceSpaces) FindTenantSpace(_ context.Context, tenant string) (Space, error) {
	if tenant != referenceTenant {
		return Space{}, Fail(SpaceNotFound, "not found")
	}
	return s.tenant, nil
}
func (s referenceSpaces) FindPlatformSpace(context.Context) (Space, error) { return s.platform, nil }

type referenceRegistry struct {
	Registry
	digest string
	called int
}

func (r *referenceRegistry) GetArtifactByDigest(_ context.Context, _, _, digest string) (Artifact, error) {
	r.called++
	if digest != r.digest {
		return Artifact{}, Fail(ImageNotFound, "missing artifact")
	}
	return Artifact{Digest: digest, MediaType: "application/vnd.oci.image.manifest.v1+json", Platforms: []ImagePlatform{{OS: "linux", Architecture: "amd64"}}}, nil
}

func referenceFixture() (*Runtime, *referenceCatalog, *referenceRegistry, context.Context) {
	digest := "sha256:" + strings.Repeat("a", 64)
	spaces := referenceSpaces{
		tenant:   Space{ID: "tenant-space", TenantID: referenceTenant, Scope: TenantImages, State: "available", ProjectID: 1, ProjectName: "t-team", RegistryAuthority: "registry.example"},
		platform: Space{ID: "platform-space", Scope: PlatformImages, State: "available", ProjectID: 2, ProjectName: "platform", RegistryAuthority: "registry.example"},
	}
	tenant := Registration{ID: "img_" + strings.Repeat("a", 32), TenantID: referenceTenant, SpaceID: spaces.tenant.ID, Scope: TenantImages, Repository: "repo", SourceReference: "registry.example/t-team/repo:latest", Digest: digest, ResolvedReference: "registry.example/t-team/repo@" + digest, MediaType: "application/vnd.oci.image.manifest.v1+json"}
	platform := tenant
	platform.ID, platform.TenantID, platform.SpaceID, platform.Scope = "img_"+strings.Repeat("b", 32), "", spaces.platform.ID, PlatformImages
	platform.SourceReference, platform.ResolvedReference = "registry.example/platform/repo:latest", "registry.example/platform/repo@"+digest
	catalog := &referenceCatalog{tenant: []Registration{tenant}, platform: []Registration{platform}}
	registry := &referenceRegistry{digest: digest}
	runtime := &Runtime{catalog: catalog, spaces: spaces, registry: registry}
	ctx := WithCaller(context.Background(), Caller{Kind: RuntimeCaller, Subject: "test", TenantID: referenceTenant})
	return runtime, catalog, registry, ctx
}

func TestResolveRegisteredReferenceUsesFrozenDigest(t *testing.T) {
	for _, scope := range []string{"t-team", "platform"} {
		t.Run(scope, func(t *testing.T) {
			runtime, _, registry, ctx := referenceFixture()
			for _, ref := range []string{"registry.example/" + scope + "/repo:latest", "registry.example/" + scope + "/repo@" + registry.digest} {
				out, err := runtime.ResolveImageReferenceForWorkload(ctx, ResolveImageReference{TenantID: referenceTenant, Reference: ref, TargetPlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}})
				if err != nil || out.Reference != "registry.example/"+scope+"/repo@"+registry.digest || out.Digest != registry.digest {
					t.Fatalf("resolve = %#v, %v", out, err)
				}
			}
			if registry.called != 2 {
				t.Fatalf("immutable artifact was not revalidated: %d", registry.called)
			}
		})
	}
}

func TestResolveFrozenDigestWithHypotheticalDuplicateRows(t *testing.T) {
	runtime, catalog, registry, ctx := referenceFixture()
	alias := catalog.tenant[0]
	alias.ID = "img_" + strings.Repeat("b", 32)
	alias.SourceReference = "registry.example/t-team/repo:another-tag"
	// Simulate abnormal duplicate rows only: the database's active digest
	// uniqueness prevents registering these two aliases concurrently. The
	// lower ID is deliberately second to test deterministic defensive behavior.
	catalog.tenant = []Registration{alias, catalog.tenant[0]}
	in := ResolveImageReference{TenantID: referenceTenant, Reference: alias.SourceReference, TargetPlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}}
	first, err := runtime.ResolveImageReferenceForWorkload(ctx, in)
	if err != nil || first.ImageID != alias.ID || first.Digest != registry.digest {
		t.Fatalf("tag resolution = %#v, %v", first, err)
	}
	in.Reference = first.Reference
	second, err := runtime.ResolveImageReferenceForWorkload(ctx, in)
	if err != nil || second.Reference != first.Reference || second.Digest != first.Digest || second.ImageID != catalog.tenant[1].ID {
		t.Fatalf("frozen digest revalidation = %#v, %v", second, err)
	}
	if registry.called != 2 {
		t.Fatalf("both resolutions must revalidate the registry artifact: %d", registry.called)
	}
}

func TestResolveReferenceRejectsUnauthorizedAmbiguousAndIncomplete(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*referenceCatalog)
		ref     string
		reason  Reason
	}{
		{"other project", nil, "registry.example/t-other/repo:latest", ImageNotFound},
		{"unregistered tag", nil, "registry.example/t-team/repo:new", ImageNotFound},
		{"unregistered digest", nil, "registry.example/t-team/repo@sha256:" + strings.Repeat("b", 64), ImageNotFound},
		{"unregistered row", func(c *referenceCatalog) { now := time.Now(); c.tenant[0].UnregisteredAt = &now }, "registry.example/t-team/repo:latest", ImageNotFound},
		{"ambiguous", func(c *referenceCatalog) {
			duplicate := c.tenant[0]
			duplicate.ID = "img_" + strings.Repeat("b", 32)
			duplicate.Digest = "sha256:" + strings.Repeat("b", 64)
			c.tenant = append(c.tenant, duplicate)
		}, "registry.example/t-team/repo:latest", ImageAlreadyRegistered},
		{"wrong tenant row", func(c *referenceCatalog) { c.tenant[0].TenantID = "22222222-2222-4222-8222-222222222222" }, "registry.example/t-team/repo:latest", InternalError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime, catalog, registry, ctx := referenceFixture()
			if test.prepare != nil {
				test.prepare(catalog)
			}
			_, err := runtime.ResolveImageReferenceForWorkload(ctx, ResolveImageReference{TenantID: referenceTenant, Reference: test.ref, TargetPlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}})
			if ReasonOf(err) != test.reason || registry.called != 0 {
				t.Fatalf("reason=%s err=%v registry reads=%d", ReasonOf(err), err, registry.called)
			}
		})
	}
}

func TestResolveReferenceIgnoresLargeUnrelatedCatalogAndFindsConflicts(t *testing.T) {
	runtime, catalog, _, ctx := referenceFixture()
	target := catalog.tenant[0]
	for i := 0; i < 150; i++ {
		unrelated := target
		unrelated.ID = fmt.Sprintf("img_%032x", i)
		unrelated.SourceReference = fmt.Sprintf("registry.example/t-team/repo:other-%d", i)
		unrelated.Digest = fmt.Sprintf("sha256:%064x", i+1)
		catalog.tenant = append(catalog.tenant, unrelated)
	}
	in := ResolveImageReference{TenantID: referenceTenant, Reference: target.SourceReference, TargetPlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}}
	if _, err := runtime.ResolveImageReferenceForWorkload(ctx, in); err != nil {
		t.Fatalf("exact match amidst large catalog: %v", err)
	}
	conflict := target
	conflict.ID = "img_" + strings.Repeat("f", 32)
	conflict.Digest = "sha256:" + strings.Repeat("f", 64)
	catalog.tenant = append(catalog.tenant, conflict)
	if _, err := runtime.ResolveImageReferenceForWorkload(ctx, in); ReasonOf(err) != ImageAlreadyRegistered {
		t.Fatalf("ambiguous tag in large catalog: %v", err)
	}
}

func TestResolveReferenceRejectsWrongCallerAndRevalidatesArtifact(t *testing.T) {
	runtime, _, registry, ctx := referenceFixture()
	in := ResolveImageReference{TenantID: referenceTenant, Reference: "registry.example/t-team/repo:latest", TargetPlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}}
	if _, err := runtime.ResolveImageReferenceForWorkload(context.Background(), in); ReasonOf(err) != TrustedCallerRequired {
		t.Fatalf("missing caller: %v", err)
	}
	registry.digest = "sha256:" + strings.Repeat("c", 64)
	if _, err := runtime.ResolveImageReferenceForWorkload(ctx, in); ReasonOf(err) != ImageNotFound || registry.called != 1 {
		t.Fatalf("artifact revalidation: %v", err)
	}
}
