package biz

import "context"

// ReferenceRuntimeImages is the narrow in-process compatibility port for
// workload owners accepting a previously registered image reference.
type ReferenceRuntimeImages interface {
	ResolveImageReferenceForWorkload(context.Context, ResolveImageReference) (ResolvedImage, error)
}

type ResolveImageReference struct {
	TenantID, Reference string
	TargetPlatform      ImagePlatform
}

// ResolveImageReferenceForWorkload resolves only a complete, unambiguous set
// of active registrations in the tenant's own or the platform image space.
// A tag denotes the digest frozen when it was registered, not the current tag
// in the registry. The selected ID goes through the normal runtime checks.
func (r *Runtime) ResolveImageReferenceForWorkload(ctx context.Context, in ResolveImageReference) (ResolvedImage, error) {
	if _, err := RequireRuntime(ctx, in.TenantID); err != nil {
		return ResolvedImage{}, err
	}
	if err := ValidateImagePlatform(in.TargetPlatform); err != nil {
		return ResolvedImage{}, err
	}
	var selected *Registration
	matchedSpaces := 0
	for _, scope := range []ImageScope{TenantImages, PlatformImages} {
		var space Space
		var err error
		if scope == TenantImages {
			space, err = r.spaces.FindTenantSpace(ctx, in.TenantID)
		} else {
			space, err = r.spaces.FindPlatformSpace(ctx)
		}
		if err != nil {
			if ReasonOf(err) == SpaceNotFound {
				continue
			}
			return ResolvedImage{}, err
		}
		ref, err := ParseImageReference(space.RegistryAuthority, space.ProjectName, in.Reference)
		if err != nil {
			continue
		}
		matchedSpaces++
		if space.Scope != scope || (scope == TenantImages && space.TenantID != in.TenantID) || (scope == PlatformImages && space.TenantID != "") || space.State != "available" || space.ProjectID <= 0 {
			return ResolvedImage{}, Fail(SpaceNotReady, "image space not ready")
		}
		// The repository returns at most two exact matches in one SQL snapshot.
		// Two are sufficient to detect an ambiguous tag. The active digest
		// index normally permits one row per repository and digest; if an
		// abnormal duplicate exists, the first two still identify the artifact.
		lookup := ReferenceLookup{SpaceID: space.ID, Repository: ref.Repository, Reference: ref.String(), IsDigest: ref.IsDigest}
		if ref.IsDigest {
			lookup.Reference = ref.Reference
		}
		var rows []Registration
		if scope == TenantImages {
			rows, err = r.catalog.FindTenantRegistrationsByReference(ctx, in.TenantID, lookup)
		} else {
			rows, err = r.catalog.FindPlatformRegistrationsByReference(ctx, lookup)
		}
		if err != nil {
			return ResolvedImage{}, err
		}
		for i := range rows {
			candidate := &rows[i]
			if err := validateRegistrationOwner(*candidate, in.TenantID, scope); err != nil {
				return ResolvedImage{}, err
			}
			if candidate.SpaceID != space.ID {
				return ResolvedImage{}, Fail(InternalError, "stored image space mismatch")
			}
			if candidate.UnregisteredAt != nil || candidate.Repository != ref.Repository {
				continue
			}
			if (ref.IsDigest && candidate.Digest != ref.Reference) || (!ref.IsDigest && candidate.SourceReference != ref.String()) {
				continue
			}
			if selected != nil {
				// Defend against abnormal duplicate rows without broadening
				// ownership to another scope, space, repository, or artifact.
				if ref.IsDigest && selected.Scope == candidate.Scope && selected.SpaceID == candidate.SpaceID && selected.Repository == candidate.Repository && selected.Digest == candidate.Digest {
					if candidate.ID < selected.ID {
						selected = candidate
					}
					continue
				}
				return ResolvedImage{}, Fail(ImageAlreadyRegistered, "image reference has multiple active registrations")
			}
			selected = candidate
		}
	}
	if matchedSpaces > 1 {
		return ResolvedImage{}, Fail(InternalError, "ambiguous image spaces")
	}
	if selected == nil {
		return ResolvedImage{}, Fail(ImageNotFound, "registered image reference not found")
	}
	return r.ResolveImageForWorkload(ctx, ResolveImage{TenantID: in.TenantID, ImageID: selected.ID, Scope: selected.Scope, TargetPlatform: in.TargetPlatform})
}

var _ ReferenceRuntimeImages = (*Runtime)(nil)
