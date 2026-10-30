package sandbox

import (
	"context"
	"errors"
	"testing"
)

type dataPlaneTestReader struct {
	sandbox Sandbox
	err     error
	reads   int
}

func (r *dataPlaneTestReader) Get(_ context.Context, _, _ string) (Sandbox, error) {
	r.reads++
	return r.sandbox, r.err
}

func TestDataPlaneTargetTenantAndLifecycle(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const id = "22222222-2222-4222-8222-222222222222"
	ctx := WithCaller(context.Background(), Caller{TenantID: tenant, Actor: "user"})
	s := Sandbox{TenantID: tenant, ID: id, Version: 1, Desired: DesiredRunning, Observed: ObservedRunning,
		Snapshot:     Snapshot{ClusterID: "cluster-a", Namespace: "tenant-test", NamespaceUID: "namespace-uid"},
		ProviderRefs: ProviderRefs{PodName: "sandbox-" + id, PodUID: "pod-uid"}}
	r := &dataPlaneTestReader{sandbox: s}
	target, err := ResolveDataPlaneTarget(ctx, r, tenant, id, DataPlaneCodeRun)
	if err != nil || target.PodUID != "pod-uid" || target.Version != 1 || target.Container != "sandbox" {
		t.Fatalf("running target = %+v, %v", target, err)
	}
	if err := RevalidateDataPlaneTarget(ctx, r, target); err != nil {
		t.Fatalf("unchanged target: %v", err)
	}

	wrongCtx := WithCaller(context.Background(), Caller{TenantID: "33333333-3333-4333-8333-333333333333", Actor: "user"})
	reads := r.reads
	if _, err := ResolveDataPlaneTarget(wrongCtx, r, tenant, id, DataPlaneFiles); !errors.Is(err, ErrDenied) || r.reads != reads {
		t.Fatalf("cross-tenant request: err=%v reads=%d", err, r.reads-reads)
	}
	if _, err := ResolveDataPlaneTarget(ctx, r, tenant, id, "admin"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown action: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Sandbox)
	}{
		{"pause intent", func(s *Sandbox) { s.Desired = DesiredPaused }},
		{"delete intent", func(s *Sandbox) { s.Desired = DesiredDeleted }},
		{"unready", func(s *Sandbox) { s.Observed = ObservedUnknown }},
		{"pending provider change", func(s *Sandbox) { s.Pending = &Mutation{Kind: MutationDeletePod} }},
		{"new version", func(s *Sandbox) { s.Version++ }},
		{"replacement pod", func(s *Sandbox) { s.ProviderRefs.PodUID = "replacement" }},
		{"replacement namespace", func(s *Sandbox) { s.Snapshot.NamespaceUID = "replacement" }},
		{"wrong pod name", func(s *Sandbox) { s.ProviderRefs.PodName = "unrelated" }},
		{"wrong tenant from repository", func(s *Sandbox) { s.TenantID = "33333333-3333-4333-8333-333333333333" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.sandbox = s
			tc.mutate(&r.sandbox)
			if err := RevalidateDataPlaneTarget(ctx, r, target); !errors.Is(err, ErrDataPlaneUnavailable) {
				t.Fatalf("stale target accepted: %v", err)
			}
		})
	}
	r.err = errors.New("database unavailable")
	if err := RevalidateDataPlaneTarget(ctx, r, target); !errors.Is(err, r.err) {
		t.Fatalf("database failure was not propagated: %v", err)
	}
}
