package instance

import (
	"context"
	"strings"

	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
)

// pullSecretStub satisfies the production port for tests that only exercise
// Notebook rendering and volume checks. It derives the real name through the
// shared rule instead of inventing one, and refuses plaintext material.
type pullSecretStub struct{}

func (pullSecretStub) ResolveImageForWorkload(_ context.Context, in imagebiz.ResolveImage) (imagebiz.ResolvedImage, error) {
	return imagebiz.ResolvedImage{TenantID: in.TenantID, ImageID: in.ImageID, Scope: in.Scope, Reference: "registry.invalid/work@sha256:" + strings.Repeat("c", 64), Digest: "sha256:" + strings.Repeat("c", 64), Platform: in.TargetPlatform, RegistryAuthority: "registry.invalid"}, nil
}

func (pullSecretStub) GetTenantPullMaterial(context.Context, string) (imagebiz.PullMaterial, error) {
	return imagebiz.PullMaterial{}, imagebiz.Fail(imagebiz.PermissionDenied, "plaintext pull material is not available to this caller")
}

func (pullSecretStub) EnsureTenantPullSecret(_ context.Context, in imagebiz.EnsurePullSecret) (imagebiz.EnsuredPullSecret, error) {
	name, err := imagebiz.PullSecretName(in.TenantID, in.ClusterID, in.Namespace, "registry.invalid")
	if err != nil {
		return imagebiz.EnsuredPullSecret{}, err
	}
	return imagebiz.EnsuredPullSecret{SecretRef: name, Generation: 1, RegistryAuthority: "registry.invalid", SpaceID: "space-test"}, nil
}
