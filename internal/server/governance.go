package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	instancepb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	sandboxpb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	computebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	instancebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	sandboxbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const GovernanceSAN = "ani-governance"
const NetworkSAN = "ani-network-service"
const OperatorHeader = "x-ani-operator"
const GovernanceScopeHeader = "x-ani-scope"

// Platform delegation is explicit and mutually exclusive with tenant delegation.
var governancePlatformMethods = map[string]struct{}{
	"/network.v1.PlatformEIPService/ListPlatformEIPs":                        {},
	"/network.v1.PlatformEIPService/GetPlatformEIP":                          {},
	"/network.v1.PlatformEIPService/ReleasePlatformEIP":                      {},
	"/network.v1.PlatformNetworkService/ListNodeInterfaces":                  {},
	"/network.v1.PlatformNetworkService/CreateVirtualSwitch":                 {},
	"/network.v1.PlatformNetworkService/GetVirtualSwitch":                    {},
	"/network.v1.PlatformNetworkService/ListVirtualSwitches":                 {},
	"/network.v1.PlatformNetworkService/DeleteVirtualSwitch":                 {},
	"/network.v1.PlatformDeviceRetirementService/RetireNetworkDevice":        {},
	"/network.v1.PlatformDeviceRetirementService/GetNetworkDeviceRetirement": {},
	"/network.v1.PlatformDefaultEgressService/CreateDefaultVPCEgress":        {},
	"/network.v1.PlatformDefaultEgressService/GetDefaultVPCEgress":           {},
	"/network.v1.PlatformDefaultEgressService/GetCurrentDefaultVPCEgress":    {},
	"/network.v1.PlatformDefaultEgressService/DeleteDefaultVPCEgress":        {},
	"/network.v1.PlatformNetworkService/AdoptNetworkDevice":                  {},
	"/network.v1.PlatformNetworkService/GetNetworkDevice":                    {},
	"/network.v1.PlatformNetworkService/CreateVlanNetwork":                   {},
	"/network.v1.PlatformNetworkService/GetVlanNetwork":                      {},
	"/network.v1.PlatformNetworkService/ListVlanNetworks":                    {},
	"/network.v1.PlatformNetworkService/DeleteVlanNetwork":                   {},
	"/network.v1.PlatformNetworkService/CreateEgressGateway":                 {},
	"/network.v1.PlatformNetworkService/GetEgressGateway":                    {},
	"/network.v1.PlatformNetworkService/ListEgressGateways":                  {},
	"/network.v1.PlatformNetworkService/DeleteEgressGateway":                 {},
	"/network.v1.PlatformNetworkService/CreatePublicAddressPool":             {},
	"/network.v1.PlatformNetworkService/GetPublicAddressPool":                {},
	"/network.v1.PlatformNetworkService/ListPublicAddressPools":              {},
	"/network.v1.PlatformNetworkService/DeletePublicAddressPool":             {},
	"/network.v1.PlatformNetworkService/RecordPublicPoolVerification":        {},
	"/network.v1.PlatformNetworkService/SetPublicPoolAllocationEnabled":      {},
	"/network.v1.PlatformNetworkService/SetDefaultPublicPool":                {},
	"/network.v1.PlatformNetworkService/CreateIntranetAddressPool":           {},
	"/network.v1.PlatformNetworkService/GetIntranetAddressPool":              {},
	"/network.v1.PlatformNetworkService/ListIntranetAddressPools":            {},
	"/network.v1.PlatformNetworkService/DeleteIntranetAddressPool":           {},
	"/network.v1.PlatformNetworkService/RecordIntranetPoolVerification":      {},
	"/network.v1.PlatformNetworkService/SetIntranetPoolAllocationEnabled":    {},
	"/network.v1.PlatformNetworkService/SetDefaultIntranetPool":              {},
	"/network.v1.PlatformNetworkService/GetPlatformNetworkCapabilities":      {},
	"/network.v1.PlatformNetworkService/GetPlatformOperation":                {},
}

// governanceReadMethods enforces the registered per-domain allowlist for the
// internal business listener. Domain = one allowlist group; adding a domain
// adds a group, never a new transport or trust contract. Keep in sync with
// docs/specs/governance-calls.md (the registered method table is authoritative).
var governanceReadMethods = map[string]struct{}{
	// Network domain: tenant VPC/Subnet read+write and operation lookup.
	"/network.v1.NetworkService/GetVPC":             {},
	"/network.v1.NetworkService/ListVPCs":           {},
	"/network.v1.NetworkService/ListVPCCIDRPresets": {},
	"/network.v1.NetworkService/CreateVPC":          {},
	"/network.v1.NetworkService/DeleteVPC":          {},
	"/network.v1.NetworkService/GetSubnet":          {},
	"/network.v1.NetworkService/ListSubnets":        {},
	"/network.v1.NetworkService/CreateSubnet":       {},
	"/network.v1.NetworkService/DeleteSubnet":       {},
	"/network.v1.NetworkService/GetOperation":       {},
	// Egress domain: tenant EIP and SNAT binding read+write.
	"/network.v1.TenantEgressService/CreateEIP":            {},
	"/network.v1.TenantEgressService/GetEIP":               {},
	"/network.v1.TenantEgressService/ListEIPs":             {},
	"/network.v1.TenantEgressService/DeleteEIP":            {},
	"/network.v1.TenantEgressService/BindVPCSnat":          {},
	"/network.v1.TenantEgressService/GetVPCSnat":           {},
	"/network.v1.TenantEgressService/GetVPCSnatBinding":    {},
	"/network.v1.TenantEgressService/SetVPCSnatEnabled":    {},
	"/network.v1.TenantEgressService/DeleteVPCSnatBinding": {},
	// Load balancer domain: tenant LB read+write.
	"/network.v1.TenantLoadBalancerService/CreateLoadBalancer":       {},
	"/network.v1.TenantLoadBalancerService/GetLoadBalancer":          {},
	"/network.v1.TenantLoadBalancerService/ListLoadBalancers":        {},
	"/network.v1.TenantLoadBalancerService/UpdateLoadBalancer":       {},
	"/network.v1.TenantLoadBalancerService/DeleteLoadBalancer":       {},
	"/network.v1.TenantLoadBalancerService/GetLoadBalancerOperation": {},
}

// storageGovernanceMethods is the tenant-bound Storage domain allowlist.
// Tenant identity is bound to the authenticated Governance scope; no platform
// scope is accepted for these methods.
var storageGovernanceMethods = map[string]struct{}{
	"/storage.v1.StorageService/ListStorageClasses": {},
	"/storage.v1.StorageService/CreateVolume":       {},
	"/storage.v1.StorageService/ListVolumes":        {},
	"/storage.v1.StorageService/GetVolume":          {},
	"/storage.v1.StorageService/UpdateVolume":       {},
	"/storage.v1.StorageService/DeleteVolume":       {},
	"/storage.v1.StorageService/ListBuckets":        {},
	"/storage.v1.StorageService/ListObjects":        {},
	"/storage.v1.StorageService/HeadObject":         {},
	"/storage.v1.StorageService/GetDownloadURL":     {},
}

// GetVPCMethod is kept for the vpc-read mode and existing deployment checks.
const GetVPCMethod = "/network.v1.NetworkService/GetVPC"

// GovernanceTLS requires a private CA, a server certificate and the fixed client SAN.
func GovernanceTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read governance CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("governance CA contains no certificates")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load network server certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	if !exactDNS(leaf, NetworkSAN) {
		return nil, fmt.Errorf("network server certificate requires exact DNS SAN ani-network-service")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if cs.Version != tls.VersionTLS13 || len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 || !exactClientDNS(cs.PeerCertificates[0], GovernanceSAN) {
				return fmt.Errorf("untrusted governance service identity")
			}
			return nil
		}}, nil
}

func exactDNS(cert *x509.Certificate, name string) bool {
	if cert == nil {
		return false
	}
	matched := false
	for _, dns := range cert.DNSNames {
		if strings.Contains(dns, "*") {
			return false
		}
		if dns == name {
			matched = true
		}
	}
	return matched
}

func exactClientDNS(cert *x509.Certificate, name string) bool {
	return cert != nil && len(cert.DNSNames) == 1 && len(cert.URIs) == 0 &&
		len(cert.IPAddresses) == 0 && len(cert.EmailAddresses) == 0 &&
		cert.DNSNames[0] == name && !strings.Contains(name, "*")
}

// This identity is asserted by the authenticated Governance workload, not IAM.
type governancePrincipal struct {
	TenantID, Actor, RequestID, Operator, Workload string
	Platform                                       bool
}
type governancePrincipalKey struct{}

type GovernanceResolver struct{}

func (GovernanceResolver) Resolve(ctx context.Context) (governancePrincipal, error) {
	denied := status.Error(codes.Unauthenticated, "trusted governance identity required")
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil {
		return governancePrincipal{}, denied
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || ti.State.Version != tls.VersionTLS13 || len(ti.State.VerifiedChains) == 0 || len(ti.State.PeerCertificates) == 0 || !exactClientDNS(ti.State.PeerCertificates[0], GovernanceSAN) {
		return governancePrincipal{}, denied
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return governancePrincipal{}, denied
	}
	value := func(key string) string {
		vals := md.Get(key)
		if len(vals) != 1 {
			return ""
		}
		return vals[0]
	}
	tenant, actor, request, operator := value("x-ani-tenant-id"), value("x-ani-actor"), value("x-ani-request-id"), value(OperatorHeader)
	validUUID := func(v string) bool { id, err := uuid.Parse(v); return err == nil && id != uuid.Nil && id.String() == v }
	scope := value(GovernanceScopeHeader)
	platform := scope == "platform"
	if len(md.Get(GovernanceScopeHeader)) > 1 || (len(md.Get(GovernanceScopeHeader)) == 1 && !platform) {
		return governancePrincipal{}, denied
	}
	if platform {
		if len(md.Get("x-ani-tenant-id")) != 0 || !strings.HasPrefix(actor, "governance:user:") {
			return governancePrincipal{}, denied
		}
	} else if !validUUID(tenant) {
		return governancePrincipal{}, denied
	}
	if !validUUID(request) || !validGovernanceActor(actor) {
		return governancePrincipal{}, denied
	}
	// Legacy operator values name the same actor; they cannot replace the
	// independently validated request ID or assert another operator.
	expectedOperator := strings.Replace(actor, "governance:user:", "gov-user-", 1)
	if strings.HasPrefix(actor, "governance:access-key:") {
		expectedOperator = strings.Replace(actor, "governance:access-key:", "gov-ak-", 1)
	}
	if len(md.Get(OperatorHeader)) > 1 || (len(md.Get(OperatorHeader)) == 1 && operator != expectedOperator) {
		return governancePrincipal{}, denied
	}
	return governancePrincipal{TenantID: tenant, Actor: actor, RequestID: request, Operator: operator, Workload: GovernanceSAN, Platform: platform}, nil
}

// Actor namespaces keep human and API Key identities distinct. Both are
// delegated by Governance; Network does not re-authorize their roles.
func validGovernanceActor(actor string) bool {
	for _, prefix := range []string{"governance:user:", "governance:access-key:"} {
		if strings.HasPrefix(actor, prefix) {
			id, err := strconv.ParseUint(strings.TrimPrefix(actor, prefix), 10, 32)
			return err == nil && id != 0 && actor == prefix+strconv.FormatUint(id, 10)
		}
	}
	return false
}

// GovernanceUnary authorizes exact methods and exclusive tenant/platform scopes
// before business handlers or SQL. Platform authority is delegated only by the
// verified Governance workload, never by public headers or protobuf fields.
func GovernanceUnary() grpc.UnaryServerInterceptor {
	return governanceUnary(false)
}

func governanceUnary(readOnly bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		p, err := (GovernanceResolver{}).Resolve(ctx)
		if err != nil {
			return nil, err
		}
		if readOnly && (info.FullMethod != GetVPCMethod || p.Platform) {
			return nil, status.Error(codes.PermissionDenied, "read-only composition permits only GetVPC")
		}
		_, platformMethod := governancePlatformMethods[info.FullMethod]
		_, computePlatformMethod := computePlatformMethods[info.FullMethod]
		platformMethod = platformMethod || computePlatformMethod
		if platformMethod != p.Platform {
			return nil, status.Error(codes.PermissionDenied, "governance scope does not permit this method")
		}
		_, imageMethod := imageGovernanceMethods[info.FullMethod]
		_, instanceMethod := instanceGovernanceMethods[info.FullMethod]
		_, computeMethod := computeGovernanceMethods[info.FullMethod]
		_, storageMethod := storageGovernanceMethods[info.FullMethod]
		_, vmMethod := vmGovernanceMethods[info.FullMethod]
		_, sandboxMethod := sandboxGovernanceMethods[info.FullMethod]
		if _, ok := governanceReadMethods[info.FullMethod]; !ok && !imageMethod && !platformMethod && !instanceMethod && !computeMethod && !storageMethod && !vmMethod && !sandboxMethod {
			return nil, status.Error(codes.PermissionDenied, "governance credential does not permit this method")
		}
		r, ok := req.(proto.Message)
		if !ok || !r.ProtoReflect().IsValid() {
			return nil, status.Error(codes.PermissionDenied, "governance requests must be proto messages")
		}
		ctx = context.WithValue(ctx, governancePrincipalKey{}, p)
		if vmMethod {
			ctx, err = withVMGovernanceScope(ctx, p, r, info.FullMethod)
			if err != nil {
				return nil, err
			}
			return next(ctx, req)
		}
		if computeMethod || computePlatformMethod {
			ctx, err = withComputeGovernanceScope(ctx, p, r, info.FullMethod)
			if err != nil {
				return nil, err
			}
			return next(ctx, req)
		}
		if sandboxMethod {
			var tenant string
			switch v := r.(type) {
			case *sandboxpb.AcceptCreateRequest:
				tenant = v.GetContext().GetTenantId()
			default:
				if fd := r.ProtoReflect().Descriptor().Fields().ByName("tenant_id"); fd != nil {
					tenant = r.ProtoReflect().Get(fd).String()
				}
			}
			if tenant == "" || tenant != p.TenantID {
				return nil, status.Error(codes.PermissionDenied, "sandbox tenant differs from authenticated scope")
			}
			if info.FullMethod == sandboxpb.SandboxService_ResolveCreateSpec_FullMethodName {
				ctx = computeScope(ctx, p, info.FullMethod, computebiz.Sandbox)
			}
			return next(sandboxbiz.WithCaller(ctx, sandboxCaller(p.TenantID, p.Actor)), req)
		}
		if instanceMethod {
			var tenant string
			switch info.FullMethod {
			case instancepb.InstanceService_AcceptCreate_FullMethodName:
				if command, ok := r.(*instancepb.AcceptCreateRequest); ok {
					tenant = command.GetContext().GetTenantId()
				}
			case instancepb.InstanceService_AcceptDelete_FullMethodName:
				if command, ok := r.(*instancepb.AcceptDeleteRequest); ok {
					tenant = command.GetContext().GetTenantId()
				}
			default:
				if fd := r.ProtoReflect().Descriptor().Fields().ByName("tenant_id"); fd != nil {
					tenant = r.ProtoReflect().Get(fd).String()
				}
			}
			if tenant == "" || tenant != p.TenantID {
				return nil, status.Error(codes.PermissionDenied, "instance tenant differs from authenticated scope")
			}
			if info.FullMethod == instancepb.InstanceService_ResolveCreateSpec_FullMethodName {
				ctx = computeScope(ctx, p, info.FullMethod, computebiz.ContainerInstance)
			}
			return next(instancebiz.WithCaller(ctx, instancebiz.Caller{TenantID: p.TenantID, Actor: p.Actor}), req)
		}
		if platformMethod {
			ctx = biz.WithEgressCaller(ctx, biz.EgressCaller{PlatformAdministrator: true, Attribution: biz.Attribution{Actor: p.Actor, DirectCaller: p.Workload, CorrelationID: p.RequestID}})
			return next(ctx, req)
		}
		if imageMethod {
			ctx, err = withImageGovernanceCaller(ctx, p, r)
			if err != nil {
				return nil, err
			}
			return next(ctx, req)
		}
		value := r.ProtoReflect()
		if fd := value.Descriptor().Fields().ByName("attribution"); fd != nil {
			if !value.Has(fd) {
				return nil, status.Error(codes.PermissionDenied, "authenticated request attribution required")
			}
			attribution := value.Get(fd).Message()
			for field, expected := range map[protoreflect.Name]string{"actor": p.Actor, "direct_caller": p.Workload, "correlation_id": p.RequestID} {
				fieldDescriptor := attribution.Descriptor().Fields().ByName(field)
				if fieldDescriptor == nil || attribution.Get(fieldDescriptor).String() != expected {
					return nil, status.Error(codes.PermissionDenied, "request attribution differs from authenticated governance scope")
				}
			}
		}
		// Bind the request's tenant field to the authenticated governance
		// scope. Empty tenant is only accepted for NetworkService reads where
		// the legacy contract sends it explicitly; Egress/LB always carry
		// target_tenant_id.
		for _, field := range []string{"tenant_id", "target_tenant_id"} {
			fd := value.Descriptor().Fields().ByName(protoreflect.Name(field))
			if fd == nil {
				continue
			}
			if got := value.Get(fd).String(); got != "" && got != p.TenantID {
				return nil, status.Error(codes.PermissionDenied, "request tenant differs from authenticated governance scope")
			}
		}
		if strings.HasPrefix(info.FullMethod, "/network.v1.NetworkService/") {
			// Keep the legacy strict behavior: NetworkService messages must
			// state the tenant explicitly and it must match the principal.
			fd := value.Descriptor().Fields().ByName("tenant_id")
			if fd != nil && value.Get(fd).String() != p.TenantID {
				return nil, status.Error(codes.PermissionDenied, "request tenant differs from authenticated governance scope")
			}
		}
		// GetOperation resolves egress/lb operations, whose authorization also
		// requires the trusted caller context.
		needsCaller := strings.HasPrefix(info.FullMethod, "/network.v1.TenantEgressService/") ||
			strings.HasPrefix(info.FullMethod, "/network.v1.TenantLoadBalancerService/") ||
			info.FullMethod == "/network.v1.NetworkService/GetOperation"
		if needsCaller {
			caller := biz.EgressCaller{
				TenantID: p.TenantID,
				Attribution: biz.Attribution{
					Actor:         p.Actor,
					DirectCaller:  p.Workload,
					CorrelationID: p.RequestID,
				},
			}
			ctx = biz.WithEgressCaller(ctx, caller)
		}
		return next(ctx, req)
	}
}

// DenyGovernanceStreams keeps the governance channel unary-only. The old name
// is retained for one release as an alias for deployed composition roots.
func DenyGovernanceStreams(_ any, _ grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
	return status.Error(codes.PermissionDenied, "governance mode does not permit streaming RPCs")
}

// DenyVPCStreams is the historical name of DenyGovernanceStreams.
//
// Deprecated: use DenyGovernanceStreams.
var DenyVPCStreams = DenyGovernanceStreams
