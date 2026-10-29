package sandbox

import (
	"context"
	"strings"
	"time"

	image "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
)

// ImageRuntime supplies the same image and pull material used by the provider.
type ImageRuntime interface {
	image.ReferenceRuntimeImages
	GetTenantPullMaterial(context.Context, string) (image.PullMaterial, error)
}

// Image delegates reference resolution and credential availability to the
// Image domain, which owns the registry and registration authorization rules.
type Image struct{ Runtime ImageRuntime }

func (i Image) ResolveSandboxImage(ctx context.Context, tenant, reference string) (string, error) {
	if i.Runtime == nil {
		return "", biz.ErrDependencyUnavailable
	}
	ctx = image.WithCaller(ctx, image.Caller{Kind: image.RuntimeCaller, Subject: "ani-resource-service", TenantID: tenant})
	resolved, err := i.Runtime.ResolveImageReferenceForWorkload(ctx, image.ResolveImageReference{TenantID: tenant, Reference: reference, TargetPlatform: image.ImagePlatform{OS: "linux", Architecture: "amd64"}})
	if err != nil {
		switch image.ReasonOf(err) {
		case image.TrustedCallerRequired, image.PermissionDenied:
			return "", biz.ErrDenied
		case image.InvalidArgument, image.InvalidReference:
			return "", biz.ErrInvalid
		case image.ImageNotFound:
			return "", biz.ErrNotFound
		case image.ImageAlreadyRegistered:
			return "", biz.ErrConflict
		default:
			return "", err
		}
	}
	// A platform image can be registered even when this tenant has no usable
	// pull identity. Check the same material the worker needs before accepting
	// a new request. Never retain the plaintext or include it in an error.
	material, err := i.Runtime.GetTenantPullMaterial(ctx, tenant)
	defer clear(material.Secret)
	if err != nil {
		return "", err
	}
	if material.TenantID != tenant || material.SpaceID == "" || material.RegistryAuthority == "" || material.Username == "" || len(material.Secret) == 0 || material.Generation < 1 || (material.ExpiresAt != nil && !material.ExpiresAt.After(time.Now())) || !strings.HasPrefix(resolved.Reference, material.RegistryAuthority+"/") {
		return "", biz.ErrDependencyUnavailable
	}
	for _, b := range material.Secret {
		if b >= 0x80 {
			return "", biz.ErrDependencyUnavailable
		}
	}
	return resolved.Reference, nil
}

var _ biz.ImageResolver = Image{}
