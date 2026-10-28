package server

import (
	"context"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var vmGovernanceMethods = map[string]struct{}{
	"/vm.v1.VMService/GetDNSDefaults":    {},
	"/vm.v1.VMService/ResolveCreateSpec": {}, "/vm.v1.VMService/AcceptCreate": {}, "/vm.v1.VMService/AcceptDelete": {},
	"/vm.v1.VMService/ListVMs": {}, "/vm.v1.VMService/GetVM": {}, "/vm.v1.VMService/GetOperation": {}, "/vm.v1.VMService/LookupCommandReplay": {},
	"/vm.v1.VMService/Start": {}, "/vm.v1.VMService/Stop": {}, "/vm.v1.VMService/ForceStop": {}, "/vm.v1.VMService/Restart": {},
	"/vm.v1.VMService/ListDisks": {}, "/vm.v1.VMService/CreateDisk": {}, "/vm.v1.VMService/AttachDisk": {}, "/vm.v1.VMService/DetachDisk": {}, "/vm.v1.VMService/DeleteDisk": {},
	"/vm.v1.VMService/SetMedia": {}, "/vm.v1.VMService/EjectMedia": {}, "/vm.v1.VMService/SetBootDevice": {}, "/vm.v1.VMService/ListMedia": {},
	"/vm.v1.VMService/ListSnapshots": {}, "/vm.v1.VMService/GetSnapshot": {}, "/vm.v1.VMService/CreateSnapshot": {}, "/vm.v1.VMService/DeleteSnapshot": {},
	"/vm.v1.VMService/PreviewRestore": {}, "/vm.v1.VMService/RestoreSnapshot": {}, "/vm.v1.VMService/ResolveConsoleTarget": {},
}

func withVMGovernanceScope(ctx context.Context, p governancePrincipal, m proto.Message, method string) (context.Context, error) {
	var tenant string
	switch r := m.(type) {
	case *pb.AcceptCreateRequest:
		tenant = r.GetContext().GetTenantId()
	case *pb.AcceptDeleteRequest:
		tenant = r.GetContext().GetTenantId()
	default:
		if f := m.ProtoReflect().Descriptor().Fields().ByName("tenant_id"); f != nil {
			tenant = m.ProtoReflect().Get(f).String()
		}
	}
	if p.Platform || tenant == "" || tenant != p.TenantID {
		return nil, status.Error(codes.PermissionDenied, "VM_SCOPE_DENIED")
	}
	if method == pb.VMService_ResolveCreateSpec_FullMethodName {
		ctx = computeScope(ctx, p, method, compute.VirtualMachine)
	}
	return biz.WithCaller(ctx, biz.Caller{TenantID: tenant, Actor: p.Actor}), nil
}
