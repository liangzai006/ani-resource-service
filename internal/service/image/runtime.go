package service

import (
	"context"

	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
)

// RuntimeService is registered only by the dedicated runtime mTLS listener.
// The Governance/catalog listener cannot deliver pull credentials.
type RuntimeService struct {
	imagev1.UnimplementedImageRuntimeServiceServer
	images biz.RuntimeImages
}

func NewRuntimeService(images biz.RuntimeImages) *RuntimeService {
	return &RuntimeService{images: images}
}
func (s *RuntimeService) ResolveImageForWorkload(ctx context.Context, r *imagev1.ResolveImageForWorkloadRequest) (*imagev1.ResolveImageForWorkloadResponse, error) {
	scope, err := readScope(r.GetScope())
	if err != nil {
		return nil, rpcError(err)
	}
	p := r.GetTargetPlatform()
	v, err := s.images.ResolveImageForWorkload(ctx, biz.ResolveImage{TenantID: r.GetTenantId(), Scope: scope, ImageID: r.GetImageId(), TargetPlatform: biz.ImagePlatform{OS: p.GetOs(), Architecture: p.GetArchitecture(), Variant: p.GetVariant()}})
	if err != nil {
		return nil, rpcError(err)
	}
	return &imagev1.ResolveImageForWorkloadResponse{TenantId: v.TenantID, ImageId: v.ImageID, Scope: wireScope(v.Scope), ResolvedReference: v.Reference, Digest: v.Digest, ImageVersion: v.Version, SelectedPlatform: wirePlatform(v.Platform), RegistryAuthority: v.RegistryAuthority}, nil
}
func (s *RuntimeService) GetTenantPullMaterial(ctx context.Context, r *imagev1.GetTenantPullMaterialRequest) (*imagev1.GetTenantPullMaterialResponse, error) {
	v, err := s.images.GetTenantPullMaterial(ctx, r.GetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	defer clear(v.Secret)
	return &imagev1.GetTenantPullMaterialResponse{TenantId: v.TenantID, SpaceId: v.SpaceID, RegistryAuthority: v.RegistryAuthority, Username: v.Username, Secret: string(v.Secret), Generation: v.Generation, ExpiresAt: optionalTime(v.ExpiresAt)}, nil
}

// EnsureTenantPullSecret exposes only the non-sensitive receipt. The response
// type has no credential field, so no plaintext can leave this surface.
func (s *RuntimeService) EnsureTenantPullSecret(ctx context.Context, r *imagev1.EnsureTenantPullSecretRequest) (*imagev1.EnsureTenantPullSecretResponse, error) {
	v, err := s.images.EnsureTenantPullSecret(ctx, biz.EnsurePullSecret{
		PullSecretScope: biz.PullSecretScope{TenantID: r.GetTenantId(), ClusterID: r.GetClusterId(), Namespace: r.GetNamespace(), RegistryAuthority: r.GetRegistryAuthority(), SpaceID: r.GetSpaceId()},
		NamespaceUID:    r.GetNamespaceUid(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	return &imagev1.EnsureTenantPullSecretResponse{SecretRef: v.SecretRef, Generation: v.Generation, ExpiresAt: optionalTime(v.ExpiresAt), RegistryAuthority: v.RegistryAuthority, SpaceId: v.SpaceID}, nil
}
