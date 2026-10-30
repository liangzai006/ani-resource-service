package instance

import (
	"context"

	image "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
)

// Image is a narrow adapter to the existing public Image use case. Instance
// code neither reads Image tables nor reconstructs registry credentials.
type Image struct{ Runtime image.RuntimeImages }

func (i Image) Resolve(ctx context.Context, tenant, id, scope string) (biz.ResolvedImage, error) {
	if i.Runtime == nil {
		return biz.ResolvedImage{}, biz.ErrInvalid
	}
	ctx = image.WithCaller(ctx, image.Caller{Kind: image.RuntimeCaller, Subject: biz.Owner, TenantID: tenant})
	r, e := i.Runtime.ResolveImageForWorkload(ctx, image.ResolveImage{TenantID: tenant, ImageID: id, Scope: image.ImageScope(scope), TargetPlatform: image.ImagePlatform{OS: "linux", Architecture: "amd64"}})
	if e != nil {
		return biz.ResolvedImage{}, e
	}
	return biz.ResolvedImage{Reference: r.Reference, Digest: r.Digest, RegistryAuthority: r.RegistryAuthority}, nil
}

// EnsureInstancePullSecret routes the instance path through the same unified
// Ensure use case the runtime RPCs use. The returned name is the frozen spec
// reference; no second writer exists for instances.
func (i Image) EnsureInstancePullSecret(ctx context.Context, tenant, cluster, namespace string) (string, error) {
	if i.Runtime == nil {
		return "", biz.ErrInvalid
	}
	ctx = image.WithCaller(ctx, image.Caller{Kind: image.RuntimeCaller, Subject: biz.Owner, TenantID: tenant})
	r, e := i.Runtime.EnsureTenantPullSecret(ctx, image.EnsurePullSecret{PullSecretScope: image.PullSecretScope{TenantID: tenant, ClusterID: cluster, Namespace: namespace}})
	if e != nil {
		return "", e
	}
	if r.SecretRef == "" {
		return "", biz.ErrInvalid
	}
	return r.SecretRef, nil
}

var _ biz.PullSecretEscape = Image{}
