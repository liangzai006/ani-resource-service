package vm

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type dnsEnvironment struct{ biz.EnvironmentPort }

func (dnsEnvironment) ResolveNamespace(context.Context, string, string) (biz.ObjectRef, error) {
	return biz.ObjectRef{Name: "tenant-dns", UID: "namespace-uid"}, nil
}
func (dnsEnvironment) ValidateStorage(context.Context, string, string) error { return nil }
func (dnsEnvironment) ValidateNetwork(_ context.Context, _, _, _ string, selection biz.NetworkSelection) (biz.NetworkSelection, error) {
	selection.NADName, selection.NADUID, selection.NADDigest = "vm-main", "nad-uid", strings.Repeat("a", 64)
	return selection, nil
}

type dnsCompute struct{ selection *compute.ComputeSelection }

func (c dnsCompute) ResolveComputePreset(context.Context, *compute.ResolveComputePresetRequest) (*compute.ComputeSelection, error) {
	return proto.Clone(c.selection).(*compute.ComputeSelection), nil
}

func dnsServiceFixture(t *testing.T) (*Service, context.Context, *pb.ResolveCreateSpecRequest) {
	t.Helper()
	tenant, cluster := uuid.NewString(), uuid.NewString()
	selection := &compute.ComputeSelection{SchemaVersion: 1, PresetId: uuid.NewString(), Code: "vm-cpu", Name: "VM CPU", ComputeType: "cpu", CpuMillis: 2000, MemoryMib: 2048}
	selection.SelectionDigest, _ = compute.SelectionDigest(selection)
	media := &pb.MediaReference{MediaKey: "linux", Kind: "DISK_IMAGE", ClusterId: cluster, Namespace: "media", PvcName: "source", PvcUid: "source-uid", Checksum: strings.Repeat("b", 64), VolumeMode: "Block", ContentType: "kubevirt", VirtualCapacityBytes: 4e9, RequestedBytes: 4e9, OsFamily: "linux", OsVersion: "24.04", Architecture: "amd64", Firmware: "bios", RootBus: "virtio", DataBus: "scsi", NicModel: "virtio"}
	s := &Service{environment: dnsEnvironment{}, compute: dnsCompute{selection}, native: &mediaServiceNative{}, config: Config{ClusterID: cluster, ServiceAccount: "workload", StorageClass: "ani-block", StorageClassUID: "sc-uid", DefaultDNSServers: []string{"223.5.5.5"}}, media: map[string]*pb.MediaReference{media.MediaKey: media}}
	start := false
	r := &pb.ResolveCreateSpecRequest{TenantId: tenant, VmContractVersion: pb.ContractVersion, Data: &pb.CreateVMData{Name: "DNS VM", PresetId: selection.PresetId, SourceKind: media.Kind, MediaKey: media.MediaKey, SystemDiskSizeGb: 8, Network: &pb.NetworkSelection{VpcId: "vpc_" + strings.ReplaceAll(uuid.NewString(), "-", ""), SubnetId: "subnet_" + strings.ReplaceAll(uuid.NewString(), "-", "")}, StartAfterCreate: &start}}
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "user:1"})
	return s, ctx, r
}

func TestDNSResolveFreezesActualDefaultsAndPersistsWithoutReResolution(t *testing.T) {
	s, ctx, request := dnsServiceFixture(t)
	resolved, e := s.ResolveCreateSpec(ctx, request)
	if e != nil || resolved.CreateSpec.Dns.Mode != pb.DNSPlatformDefault || !slices.Equal(resolved.CreateSpec.Dns.Nameservers, []string{"223.5.5.5"}) {
		t.Fatal("default DNS was not frozen", resolved, e)
	}
	s.config.DefaultDNSServers[0] = "1.1.1.1"
	defaults, e := s.GetDNSDefaults(ctx, &pb.GetDNSDefaultsRequest{TenantId: request.TenantId})
	if e != nil || !slices.Equal(defaults.Nameservers, []string{"1.1.1.1"}) {
		t.Fatal("defaults endpoint did not show current configuration", e)
	}
	// Acceptance/replay and durable VM reload consume only the frozen spec.
	s.config.DefaultDNSServers = nil
	domain, e := domainSpec(uuid.NewString(), resolved.CreateSpec)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(domain)
	if e != nil {
		t.Fatal(e)
	}
	var reloaded biz.Spec
	if json.Unmarshal(raw, &reloaded) != nil || reloaded.DNS == nil || !slices.Equal(reloaded.DNS.Nameservers, []string{"223.5.5.5"}) {
		t.Fatal("frozen DNS did not survive durable spec serialization")
	}
	if digest, e := pb.CreateSpecDigest(resolved.CreateSpec); e != nil || digest != resolved.VmCreateDigest {
		t.Fatal("platform change rewrote frozen digest", e)
	}
	if _, e = s.ResolveCreateSpec(ctx, request); status.Code(e) != codes.Unavailable {
		t.Fatal("missing platform DNS silently fell back to cluster DNS", e)
	}
	if _, e = s.GetDNSDefaults(ctx, &pb.GetDNSDefaultsRequest{TenantId: request.TenantId}); status.Code(e) != codes.Unavailable {
		t.Fatal("invalid defaults were presented as usable", e)
	}
}

func TestDNSCustomSelectionAndDefaultsAuthorization(t *testing.T) {
	s, ctx, request := dnsServiceFixture(t)
	s.config.DefaultDNSServers = nil
	request.Data.Dns = &pb.DNSSelection{Mode: pb.DNSCustom, Nameservers: []string{"2001:0db8::1", "::ffff:223.5.5.5", "223.5.5.5"}}
	resolved, e := s.ResolveCreateSpec(ctx, request)
	if e != nil || resolved.CreateSpec.Dns.Mode != pb.DNSCustom || !slices.Equal(resolved.CreateSpec.Dns.Nameservers, []string{"2001:db8::1", "223.5.5.5"}) || !slices.Equal(resolved.CreateSpec.Intent.Dns.Nameservers, resolved.CreateSpec.Dns.Nameservers) {
		t.Fatal("custom DNS was not normalized and frozen", e)
	}
	if len(request.Data.Dns.Nameservers) != 3 {
		t.Fatal("resolution modified caller-owned intent")
	}
	if _, e = s.GetDNSDefaults(context.Background(), &pb.GetDNSDefaultsRequest{TenantId: request.TenantId}); status.Code(e) != codes.PermissionDenied {
		t.Fatal("defaults exposed without trusted tenant scope", e)
	}
	if _, e = s.GetDNSDefaults(ctx, &pb.GetDNSDefaultsRequest{TenantId: uuid.NewString()}); status.Code(e) != codes.PermissionDenied {
		t.Fatal("cross-tenant defaults request accepted", e)
	}
}
