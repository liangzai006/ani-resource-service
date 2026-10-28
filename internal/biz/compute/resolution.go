package compute

import (
	"context"
	"strconv"
	"strings"
)

type Usage uint32

const (
	UsageUnspecified Usage = iota
	ContainerInstance
	Training
	Inference
	VirtualMachine
	Sandbox
)

func ValidUsage(u Usage) bool { return u >= ContainerInstance && u <= Sandbox }

const ResolveMethod = "/compute.v1.ComputePresetService/ResolveComputePreset"
const ListMethod = "/compute.v1.ComputePresetService/ListPresets"
const ContainerResolveMethod = "/instance.v1.InstanceService/ResolveCreateSpec"
const VMResolveMethod = "/vm.v1.VMService/ResolveCreateSpec"
const SandboxResolveMethod = "/sandbox.v1.SandboxService/ResolveCreateSpec"

var platformMethods = map[string]struct{}{
	"/compute.v1.ComputePresetService/AdminListPresets":      {},
	"/compute.v1.ComputePresetService/AdminGetPreset":        {},
	"/compute.v1.ComputePresetService/AdminCreatePreset":     {},
	"/compute.v1.ComputePresetService/AdminUpdatePreset":     {},
	"/compute.v1.ComputePresetService/AdminSetPresetEnabled": {},
	"/compute.v1.ComputePresetService/AdminDeletePreset":     {},
}

// Scope is installed only after the transport has authenticated the exact caller,
// RPC and single-value tenant/actor/request metadata. Usage is an intent bound
// to that trusted delegation, never an authorization supplied by public JSON.
type Scope struct {
	TenantID, Actor, RequestID, Workload, Method string
	Platform                                     bool
	Usage                                        Usage
}
type scopeKey struct{}

func WithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}
func validActor(a string, platform bool) bool {
	prefixes := []string{"governance:user:"}
	if !platform {
		prefixes = append(prefixes, "governance:access-key:")
	}
	for _, p := range prefixes {
		if strings.HasPrefix(a, p) {
			n, e := strconv.ParseUint(strings.TrimPrefix(a, p), 10, 32)
			return e == nil && n > 0 && a == p+strconv.FormatUint(n, 10)
		}
	}
	return false
}
func ScopeFromContext(ctx context.Context) (Scope, error) {
	s, ok := ctx.Value(scopeKey{}).(Scope)
	if !ok || s.Workload != "ani-governance" || !UUID(s.RequestID) || !validActor(s.Actor, s.Platform) {
		return Scope{}, ErrDenied
	}
	if s.Platform {
		if s.TenantID != "" || s.Usage != UsageUnspecified {
			return Scope{}, ErrDenied
		}
		if _, ok := platformMethods[s.Method]; !ok {
			return Scope{}, ErrDenied
		}
	} else {
		if !UUID(s.TenantID) || !ValidUsage(s.Usage) {
			return Scope{}, ErrDenied
		}
		switch s.Method {
		case ResolveMethod, ListMethod:
		case ContainerResolveMethod:
			if s.Usage != ContainerInstance {
				return Scope{}, ErrDenied
			}
		case VMResolveMethod:
			if s.Usage != VirtualMachine {
				return Scope{}, ErrDenied
			}
		case SandboxResolveMethod:
			if s.Usage != Sandbox {
				return Scope{}, ErrDenied
			}
		default:
			return Scope{}, ErrDenied
		}
	}
	return s, nil
}
func CallerFromContext(ctx context.Context) (Scope, error) {
	s, e := ScopeFromContext(ctx)
	if e != nil || s.Platform {
		return Scope{}, ErrDenied
	}
	return s, nil
}
func RequirePlatform(ctx context.Context) (Scope, error) {
	s, e := ScopeFromContext(ctx)
	if e != nil || !s.Platform {
		return Scope{}, ErrDenied
	}
	return s, nil
}
func RequireTenant(ctx context.Context, tenant string, u Usage) error {
	s, e := CallerFromContext(ctx)
	if e != nil || !UUID(tenant) || s.TenantID != tenant || !ValidUsage(u) || s.Usage != u {
		return ErrDenied
	}
	return nil
}

type Selection struct {
	PresetID, Code, Name, ComputeType string
	CPUMillis, MemoryMiB              int64
	GPU                               *GPUReference
}
type Resolver struct {
	repo      PresetRepository
	clusterID string
}

func NewResolver(r PresetRepository, clusterID string) (*Resolver, error) {
	if r == nil || !UUID(clusterID) {
		return nil, ErrInvalid
	}
	return &Resolver{repo: r, clusterID: clusterID}, nil
}
func (r *Resolver) Resolve(ctx context.Context, tenant, presetID string, u Usage) (Selection, error) {
	if !ValidUsage(u) || !UUID(presetID) {
		return Selection{}, ErrInvalid
	}
	if e := RequireTenant(ctx, tenant, u); e != nil {
		return Selection{}, e
	}
	s, _ := CallerFromContext(ctx)
	if s.Method != ResolveMethod && s.Method != ContainerResolveMethod && s.Method != VMResolveMethod && s.Method != SandboxResolveMethod {
		return Selection{}, ErrDenied
	}
	p, e := r.repo.GetPreset(ctx, presetID)
	if e != nil {
		return Selection{}, e
	}
	if !p.Enabled {
		return Selection{}, ErrPresetDisabled
	}
	if e = ValidatePreset(p); e != nil {
		return Selection{}, e
	}
	if p.GPU != nil && p.GPU.ClusterID != r.clusterID {
		return Selection{}, ErrInvalid
	}
	out := Selection{PresetID: p.ID, Code: p.Code, Name: p.Name, ComputeType: p.ComputeType, CPUMillis: p.CPUMillis, MemoryMiB: p.MemoryMiB}
	if p.GPU != nil {
		g := *p.GPU
		out.GPU = &g
	}
	return out, nil
}
