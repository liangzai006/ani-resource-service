package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// PullSecretScope is the only identity of a shared tenant pull Secret. The
// workload kind deliberately does not participate: instances, inference and
// training in the same (tenant, cluster, namespace, registry) share one object.
type PullSecretScope struct {
	TenantID, ClusterID, Namespace, RegistryAuthority, SpaceID string
}

// EnsurePullSecret is the request a trusted runtime caller sends. Namespace and
// its UID are asserted by the caller and independently verified here.
type EnsurePullSecret struct {
	PullSecretScope
	NamespaceUID string
}

// EnsuredPullSecret is the non-sensitive receipt. It never carries a username,
// password or dockerconfig payload.
type EnsuredPullSecret struct {
	SecretRef, RegistryAuthority, SpaceID string
	Generation                            int64
	ExpiresAt                             *time.Time
}

// PullSecretName is the single naming rule. cluster_id must stay in the hash:
// the same tenant namespace name in two clusters would otherwise collide.
func PullSecretName(tenant, cluster, namespace, registry string) (string, error) {
	parts := []string{strings.ToLower(tenant), strings.ToLower(cluster), strings.ToLower(namespace), normalizeRegistryAuthority(registry)}
	for _, part := range parts {
		if part == "" {
			return "", Fail(InvalidArgument, "pull secret scope requires tenant, cluster, namespace and registry")
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "ani-pull-" + hex.EncodeToString(sum[:16]), nil
}

// normalizeRegistryAuthority lowercases the authority and drops any scheme,
// path and the default registry port, so that peers cannot disagree on the hash
// input. The registry is always HTTPS in this design, so 443 is the default.
func normalizeRegistryAuthority(authority string) string {
	a := strings.TrimSpace(strings.ToLower(authority))
	if i := strings.Index(a, "://"); i >= 0 {
		a = a[i+3:]
	}
	if i := strings.IndexAny(a, "/?#"); i >= 0 {
		a = a[:i]
	}
	if strings.HasSuffix(a, ":443") {
		a = strings.TrimSuffix(a, ":443")
	}
	return a
}

// PullSecretWriter is Resource's own Kubernetes writer. It is the single
// synchronous owner of the shared Secret and never returns plaintext.
type PullSecretWriter interface {
	// EnsurePullSecret creates or updates the Secret for this target and returns
	// the generation that is now stored. The adapter must reject an object owned
	// by another manager and any downgrade of an existing generation.
	EnsurePullSecret(context.Context, PullSecretTarget, Secret) (int64, error)
}

// PullSecretTarget carries the fully validated destination.
type PullSecretTarget struct {
	ClusterID, Namespace, NamespaceUID, SecretName, TenantID, SpaceID, RegistryAuthority string
	Generation                                                                           int64
	Username                                                                             string
}

// EnsureTenantPullSecret synchronously writes the shared Secret through
// Resource's own credentials and returns only the reference and non-sensitive
// state. Duplicate calls are idempotent: the name is derived, never supplied.
// An empty registry authority is only accepted on the internal instance path,
// where the tenant space is itself the authority.
func (r *Runtime) EnsureTenantPullSecret(ctx context.Context, in EnsurePullSecret) (EnsuredPullSecret, error) {
	caller, err := RequireRuntime(ctx, in.TenantID)
	if err != nil {
		return EnsuredPullSecret{}, err
	}
	if r.writer == nil || r.namespaces == nil {
		return EnsuredPullSecret{}, Fail(DependencyUnavailable, "pull secret writer not configured")
	}
	if err = r.guardNamespace(ctx, caller, in); err != nil {
		return EnsuredPullSecret{}, err
	}
	space, err := r.spaces.FindTenantSpace(ctx, in.TenantID)
	if err != nil {
		return EnsuredPullSecret{}, err
	}
	if space.Scope != TenantImages || space.TenantID != in.TenantID || space.State != "available" || space.ProjectID <= 0 {
		return EnsuredPullSecret{}, Fail(SpaceNotReady, "tenant image space not ready")
	}
	if in.RegistryAuthority == "" {
		if caller.Subject != instanceOwnerSubject {
			return EnsuredPullSecret{}, Fail(InvalidArgument, "registry authority required")
		}
	} else if normalizeRegistryAuthority(space.RegistryAuthority) != normalizeRegistryAuthority(in.RegistryAuthority) {
		return EnsuredPullSecret{}, Fail(SpaceNotReady, "tenant image space is not for the requested registry")
	}
	if in.SpaceID != "" && in.SpaceID != space.ID {
		return EnsuredPullSecret{}, Fail(PermissionDenied, "pull secret space differs from tenant space")
	}
	name, err := PullSecretName(in.TenantID, in.ClusterID, in.Namespace, space.RegistryAuthority)
	if err != nil {
		return EnsuredPullSecret{}, err
	}
	stored, err := r.credentials.GetTenantPull(ctx, in.TenantID, space.ID)
	if err != nil {
		return EnsuredPullSecret{}, err
	}
	info := stored.Info
	if info.Scope != TenantImages || info.TenantID != in.TenantID || info.SpaceID != space.ID || info.Purpose != "pull" || info.State != "active" || info.Generation <= 0 || info.Username == "" || info.RobotID <= 0 || (info.ExpiresAt != nil && !info.ExpiresAt.After(r.now())) {
		return EnsuredPullSecret{}, Fail(SpaceNotReady, "tenant pull credential not active")
	}
	aad := SecretAAD{InstallationID: space.InstallationID, TenantID: in.TenantID, SpaceID: space.ID, Purpose: "pull", Scope: TenantImages, Generation: info.Generation}
	if stored.AAD != aad {
		return EnsuredPullSecret{}, Fail(InternalError, "pull secret binding mismatch")
	}
	plain, err := r.cipher.Open(ctx, aad, stored.Secret)
	if err != nil {
		return EnsuredPullSecret{}, err
	}
	defer clear(plain)
	generation, err := r.writer.EnsurePullSecret(ctx, PullSecretTarget{
		ClusterID: in.ClusterID, Namespace: in.Namespace, NamespaceUID: in.NamespaceUID,
		SecretName: name, TenantID: in.TenantID, SpaceID: space.ID,
		RegistryAuthority: space.RegistryAuthority, Generation: info.Generation, Username: info.Username,
	}, plain)
	if err != nil {
		return EnsuredPullSecret{}, err
	}
	if generation < info.Generation {
		return EnsuredPullSecret{}, Fail(InternalError, "pull secret generation regressed")
	}
	return EnsuredPullSecret{SecretRef: name, Generation: generation, ExpiresAt: info.ExpiresAt, RegistryAuthority: space.RegistryAuthority, SpaceID: space.ID}, nil
}
