package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	image "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
)

type referenceRuntime struct {
	result     image.ResolvedImage
	err        error
	got        image.ResolveImageReference
	caller     image.Caller
	pull       image.PullMaterial
	pullErr    error
	pullTenant string
	pullCaller image.Caller
}

func (r *referenceRuntime) ResolveImageReferenceForWorkload(ctx context.Context, in image.ResolveImageReference) (image.ResolvedImage, error) {
	r.got = in
	r.caller, _ = image.CallerFromContext(ctx)
	return r.result, r.err
}

func (r *referenceRuntime) GetTenantPullMaterial(ctx context.Context, tenant string) (image.PullMaterial, error) {
	r.pullTenant = tenant
	r.pullCaller, _ = image.CallerFromContext(ctx)
	return r.pull, r.pullErr
}

func validPull(tenant string) image.PullMaterial {
	return image.PullMaterial{TenantID: tenant, SpaceID: "space", RegistryAuthority: "registry.example", Username: "robot", Secret: image.Secret("secret"), Generation: 1}
}

func TestImageAdapterUsesTrustedTenantAndPreservesImmutableResult(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	runtime := &referenceRuntime{result: image.ResolvedImage{Reference: "registry.example/project/repo@sha256:abc", Digest: "sha256:abc"}, pull: validPull(tenant)}
	got, err := (Image{Runtime: runtime}).ResolveSandboxImage(context.Background(), tenant, "registry.example/project/repo:tag")
	if err != nil || got != runtime.result.Reference {
		t.Fatalf("got %q, %v", got, err)
	}
	if runtime.got.TenantID != tenant || runtime.got.Reference != "registry.example/project/repo:tag" || runtime.got.TargetPlatform != (image.ImagePlatform{OS: "linux", Architecture: "amd64"}) || runtime.caller.Kind != image.RuntimeCaller || runtime.caller.TenantID != tenant || runtime.caller.Subject != "ani-resource-service" {
		t.Fatalf("request=%#v caller=%#v", runtime.got, runtime.caller)
	}
	if runtime.pullTenant != tenant || runtime.pullCaller != runtime.caller || strings.Trim(string(runtime.pull.Secret), "\x00") != "" {
		t.Fatal("pull credential must use the same tenant identity and be cleared")
	}
}

func TestImageAdapterRejectsUnavailableOrInvalidPullMaterialAndClearsSecret(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	expired := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name   string
		change func(*referenceRuntime)
	}{
		{"tenant image space absent", func(r *referenceRuntime) { r.pullErr = image.Fail(image.SpaceNotFound, "no space") }},
		{"credential unavailable", func(r *referenceRuntime) { r.pullErr = image.Fail(image.SpaceNotReady, "inactive credential") }},
		{"tenant mismatch", func(r *referenceRuntime) { r.pull.TenantID = "other" }},
		{"space missing", func(r *referenceRuntime) { r.pull.SpaceID = "" }},
		{"registry mismatch", func(r *referenceRuntime) { r.pull.RegistryAuthority = "other.example" }},
		{"username missing", func(r *referenceRuntime) { r.pull.Username = "" }},
		{"generation invalid", func(r *referenceRuntime) { r.pull.Generation = 0 }},
		{"expired", func(r *referenceRuntime) { r.pull.ExpiresAt = &expired }},
		{"nonascii", func(r *referenceRuntime) { r.pull.Secret = image.Secret{0x80, 0x81} }},
		{"empty secret", func(r *referenceRuntime) { r.pull.Secret = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &referenceRuntime{result: image.ResolvedImage{Reference: "registry.example/platform/repo@sha256:abc"}, pull: validPull(tenant)}
			tc.change(runtime)
			got, err := (Image{Runtime: runtime}).ResolveSandboxImage(context.Background(), tenant, runtime.result.Reference)
			if err == nil || got != "" {
				t.Fatalf("invalid pull material accepted: %q, %v", got, err)
			}
			if runtime.pullErr != nil && !errors.Is(err, runtime.pullErr) {
				t.Fatalf("pull error was lost: %v", err)
			}
			for _, b := range runtime.pull.Secret {
				if b != 0 {
					t.Fatal("pull secret was not cleared")
				}
			}
		})
	}
}

func TestImageAdapterMapsRegistrationErrors(t *testing.T) {
	if _, err := (Image{}).ResolveSandboxImage(context.Background(), "tenant", "ref"); err != biz.ErrDependencyUnavailable {
		t.Fatalf("missing Image runtime: got %v", err)
	}
	for reason, want := range map[image.Reason]error{image.ImageNotFound: biz.ErrNotFound, image.ImageAlreadyRegistered: biz.ErrConflict, image.PermissionDenied: biz.ErrDenied, image.InvalidReference: biz.ErrInvalid} {
		runtime := &referenceRuntime{err: image.Fail(reason, "test")}
		if _, err := (Image{Runtime: runtime}).ResolveSandboxImage(context.Background(), "tenant", "ref"); err != want {
			t.Fatalf("%s: got %v, want %v", reason, err, want)
		}
	}
}
