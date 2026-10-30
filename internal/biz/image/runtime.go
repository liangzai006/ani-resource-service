package biz

import (
	"context"
	"regexp"
	"time"
)

var platformToken = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

func ValidateImagePlatform(p ImagePlatform) error {
	if p.OS == "unknown" || p.Architecture == "unknown" || !platformToken.MatchString(p.OS) || !platformToken.MatchString(p.Architecture) || (p.Variant != "" && !platformToken.MatchString(p.Variant)) {
		return Fail(InvalidArgument, "explicit runnable target platform required")
	}
	return nil
}
func validateArtifact(a Artifact) error {
	if ParseDigest(a.Digest) != nil || len(a.Platforms) == 0 || len(a.Platforms) > 32 {
		return Fail(UnsupportedArtifact, "invalid runnable artifact")
	}
	switch a.MediaType {
	case "application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json":
	default:
		return Fail(UnsupportedArtifact, "unsupported image media type")
	}
	for _, p := range a.Platforms {
		if ValidateImagePlatform(p) != nil {
			return Fail(UnsupportedArtifact, "artifact platform unconfirmed")
		}
	}
	return nil
}

type Runtime struct {
	catalog     CatalogRepository
	spaces      SpaceRepository
	credentials CredentialRepository
	registry    Registry
	cipher      SecretCipher
	now         func() time.Time
	writer      PullSecretWriter
	namespaces  NamespaceAuthority
	callers     map[string]PullSecretCaller
}

func NewRuntime(catalog CatalogRepository, spaces SpaceRepository, credentials CredentialRepository, registry Registry, cipher SecretCipher, now func() time.Time) (*Runtime, error) {
	if catalog == nil || spaces == nil || credentials == nil || registry == nil || cipher == nil {
		return nil, Fail(InvalidArgument, "runtime image dependencies required")
	}
	if now == nil {
		now = time.Now
	}
	return &Runtime{catalog: catalog, spaces: spaces, credentials: credentials, registry: registry, cipher: cipher, now: now}, nil
}
func (r *Runtime) ResolveImageForWorkload(ctx context.Context, in ResolveImage) (ResolvedImage, error) {
	if _, err := RequireRuntime(ctx, in.TenantID); err != nil {
		return ResolvedImage{}, err
	}
	if err := ValidateImagePlatform(in.TargetPlatform); err != nil {
		return ResolvedImage{}, err
	}
	registration, err := readRegistration(ctx, r.catalog, ReadImage{TenantID: in.TenantID, ImageID: in.ImageID, Scope: in.Scope})
	if err != nil {
		return ResolvedImage{}, err
	}
	if registration.UnregisteredAt != nil {
		return ResolvedImage{}, Fail(ImageNotFound, "image is unregistered")
	}
	var space Space
	if in.Scope == TenantImages {
		space, err = r.spaces.FindTenantSpace(ctx, in.TenantID)
	} else {
		space, err = r.spaces.FindPlatformSpace(ctx)
	}
	if err != nil {
		return ResolvedImage{}, err
	}
	if space.ID != registration.SpaceID || space.Scope != in.Scope || space.TenantID != registration.TenantID || space.State != "available" || space.ProjectID <= 0 {
		return ResolvedImage{}, Fail(SpaceNotReady, "image space not ready")
	}
	ref, err := ParseImageReference(space.RegistryAuthority, space.ProjectName, registration.ResolvedReference)
	if err != nil || !ref.IsDigest || ref.Reference != registration.Digest || ref.Repository != registration.Repository {
		return ResolvedImage{}, Fail(InternalError, "stored immutable reference mismatch")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	artifact, err := r.registry.GetArtifactByDigest(ctx, space.ProjectName, registration.Repository, registration.Digest)
	if err != nil {
		return ResolvedImage{}, err
	}
	if err = validateArtifact(artifact); err != nil {
		return ResolvedImage{}, err
	}
	if artifact.Digest != registration.Digest || artifact.MediaType != registration.MediaType {
		return ResolvedImage{}, Fail(DependencyUnavailable, "registry immutable artifact mismatch")
	}
	matches := []ImagePlatform{}
	seen := map[ImagePlatform]bool{}
	for _, p := range artifact.Platforms {
		if p.OS == in.TargetPlatform.OS && p.Architecture == in.TargetPlatform.Architecture && (in.TargetPlatform.Variant == "" || p.Variant == in.TargetPlatform.Variant) && !seen[p] {
			matches = append(matches, p)
			seen[p] = true
		}
	}
	if len(matches) != 1 {
		return ResolvedImage{}, Fail(PlatformMismatch, "image does not confirm one matching target platform")
	}
	return ResolvedImage{TenantID: in.TenantID, ImageID: registration.ID, Scope: in.Scope, Reference: registration.ResolvedReference, Digest: registration.Digest, Version: registration.Version, Platform: matches[0], RegistryAuthority: space.RegistryAuthority}, nil
}
func (r *Runtime) GetTenantPullMaterial(ctx context.Context, tenant string) (PullMaterial, error) {
	if _, err := RequireRuntime(ctx, tenant); err != nil {
		return PullMaterial{}, err
	}
	space, err := r.spaces.FindTenantSpace(ctx, tenant)
	if err != nil {
		return PullMaterial{}, err
	}
	if space.Scope != TenantImages || space.TenantID != tenant || space.State != "available" || space.ProjectID <= 0 {
		return PullMaterial{}, Fail(SpaceNotReady, "tenant image space not ready")
	}
	stored, err := r.credentials.GetTenantPull(ctx, tenant, space.ID)
	if err != nil {
		return PullMaterial{}, err
	}
	info := stored.Info
	if info.Scope != TenantImages || info.TenantID != tenant || info.SpaceID != space.ID || info.Purpose != "pull" || info.State != "active" || info.Generation <= 0 || info.Username == "" || info.RobotID <= 0 || (info.ExpiresAt != nil && !info.ExpiresAt.After(r.now())) {
		return PullMaterial{}, Fail(SpaceNotReady, "tenant pull credential not active")
	}
	aad := SecretAAD{InstallationID: space.InstallationID, TenantID: tenant, SpaceID: space.ID, Purpose: "pull", Scope: TenantImages, Generation: info.Generation}
	if stored.AAD != aad {
		return PullMaterial{}, Fail(InternalError, "pull secret binding mismatch")
	}
	plain, err := r.cipher.Open(ctx, aad, stored.Secret)
	if err != nil {
		return PullMaterial{}, err
	}
	return PullMaterial{TenantID: tenant, SpaceID: space.ID, RegistryAuthority: space.RegistryAuthority, Username: info.Username, Secret: plain, Generation: info.Generation, ExpiresAt: info.ExpiresAt}, nil
}

var _ RuntimeImages = (*Runtime)(nil)
