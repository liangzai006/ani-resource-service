package server

import (
	"context"
	"testing"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestVirtualMachineComputeDelegationKeepsTenantAndMethod(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	principal := governancePrincipal{TenantID: tenant, Actor: "governance:user:42", RequestID: "22222222-2222-4222-8222-222222222222", Workload: "ani-governance"}
	for _, tc := range []struct {
		name   string
		method string
		body   proto.Message
		code   codes.Code
	}{
		{"resolve", biz.ResolveMethod, &pb.ResolveComputePresetRequest{TenantId: tenant, Usage: pb.Usage_VIRTUAL_MACHINE}, codes.OK},
		{"list", biz.ListMethod, &pb.ListPresetsRequest{TenantId: tenant, Usage: pb.Usage_VIRTUAL_MACHINE}, codes.OK},
		{"wrong tenant", biz.ResolveMethod, &pb.ResolveComputePresetRequest{TenantId: principal.RequestID, Usage: pb.Usage_VIRTUAL_MACHINE}, codes.PermissionDenied},
		{"wrong method", biz.ResolveMethod, &pb.ListPresetsRequest{TenantId: tenant, Usage: pb.Usage_VIRTUAL_MACHINE}, codes.PermissionDenied},
		{"unknown usage", biz.ResolveMethod, &pb.ResolveComputePresetRequest{TenantId: tenant, Usage: pb.Usage(6)}, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := withComputeGovernanceScope(context.Background(), principal, tc.body, tc.method)
			if status.Code(err) != tc.code {
				t.Fatal("compute delegation result", err)
			}
			if tc.code == codes.OK {
				if err := biz.RequireTenant(ctx, tenant, biz.VirtualMachine); err != nil {
					t.Fatal("VM scope not preserved", err)
				}
			}
		})
	}
}
