package compute

import (
	"context"
	"errors"
	"testing"
)

func TestVirtualMachineScopeIsExplicitAndKeepsExistingUsages(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const request = "22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		name   string
		method string
		usage  Usage
		denied bool
	}{
		{"container public", ResolveMethod, ContainerInstance, false},
		{"training public", ResolveMethod, Training, false},
		{"inference public", ResolveMethod, Inference, false},
		{"VM public", ResolveMethod, VirtualMachine, false},
		{"VM directory", ListMethod, VirtualMachine, false},
		{"container internal", ContainerResolveMethod, ContainerInstance, false},
		{"VM internal", VMResolveMethod, VirtualMachine, false},
		{"VM cannot borrow container", ContainerResolveMethod, VirtualMachine, true},
		{"container cannot borrow VM", VMResolveMethod, ContainerInstance, true},
		{"training cannot borrow VM", VMResolveMethod, Training, true},
		{"missing usage", ResolveMethod, UsageUnspecified, true},
		{"unknown usage", ResolveMethod, Sandbox + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithScope(context.Background(), Scope{TenantID: tenant, Actor: "governance:user:42", RequestID: request, Workload: "ani-governance", Method: tc.method, Usage: tc.usage})
			err := RequireTenant(ctx, tenant, tc.usage)
			if errors.Is(err, ErrDenied) != tc.denied {
				t.Fatalf("scope result: %v", err)
			}
			if !tc.denied && RequireTenant(ctx, request, tc.usage) == nil {
				t.Fatal("accepted a different tenant")
			}
		})
	}
}
