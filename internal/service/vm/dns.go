package vm

import (
	"context"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

func (s *Service) resolveDNS(selection *pb.DNSSelection) (*pb.FrozenDNS, error) {
	selection, e := pb.NormalizeDNSSelection(selection)
	if e != nil {
		return nil, biz.ErrInvalid
	}
	if selection != nil {
		return &pb.FrozenDNS{Mode: pb.DNSCustom, Nameservers: selection.Nameservers}, nil
	}
	servers, e := pb.NormalizeDNSServers(s.config.DefaultDNSServers)
	if e != nil {
		// A missing or malformed platform configuration must never silently
		// fall back to cluster DNS, which need not be reachable by this VPC.
		return nil, biz.ErrDependency
	}
	return &pb.FrozenDNS{Mode: pb.DNSPlatformDefault, Nameservers: servers}, nil
}

func (s *Service) GetDNSDefaults(ctx context.Context, r *pb.GetDNSDefaultsRequest) (*pb.DNSDefaults, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	dns, e := s.resolveDNS(nil)
	if e != nil {
		return nil, rpcError(e)
	}
	return &pb.DNSDefaults{Nameservers: dns.Nameservers}, nil
}
