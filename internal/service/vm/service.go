package vm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	compute "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	console "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/console/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type ComputeResolver interface {
	ResolveComputePreset(context.Context, *compute.ResolveComputePresetRequest) (*compute.ComputeSelection, error)
}
type AddressReader interface {
	CurrentAddresses(context.Context, biz.VM) ([]biz.Address, error)
}
type Config struct {
	ClusterID, ServiceAccount, StorageClass, StorageClassUID, BackendStorageClass, MediaStorageClass string
	Media                                                                                            []*pb.MediaReference
	DefaultDNSServers                                                                                []string
	LogLimits                                                                                        biz.LogLimits
}
type Service struct {
	pb.UnimplementedVMServiceServer
	console.UnimplementedVMConsoleServiceServer
	repo        biz.Repository
	compute     ComputeResolver
	environment biz.EnvironmentPort
	native      biz.NativePort
	config      Config
	media       map[string]*pb.MediaReference
	addresses   AddressReader
	logs        biz.LogPort
	logMu       sync.Mutex
	logTotal    int
	logTenants  map[string]int
	logVMs      map[string]int
}

func (s *Service) SetAddressReader(r AddressReader) { s.addresses = r }
func (s *Service) project(ctx context.Context, v biz.VM) (*pb.VirtualMachine, error) {
	if s.addresses != nil {
		addresses, e := s.addresses.CurrentAddresses(ctx, v)
		v.Addresses = addresses
		if e != nil {
			v.Addresses = nil
			v.Reason = "VM_DEPENDENCY_NOT_READY"
		}
	}
	return wireVM(v)
}
func NewService(r biz.Repository, c ComputeResolver, e biz.EnvironmentPort, n biz.NativePort, cfg Config) (*Service, error) {
	if r == nil || c == nil || e == nil || n == nil || !biz.UUID(cfg.ClusterID) || cfg.ServiceAccount == "" || cfg.StorageClass == "" || cfg.StorageClassUID == "" || biz.ValidateLogLimits(cfg.LogLimits) != nil {
		return nil, biz.ErrInvalid
	}
	logs, _ := n.(biz.LogPort)
	s := &Service{repo: r, compute: c, environment: e, native: n, config: cfg, media: map[string]*pb.MediaReference{}, logs: logs, logTenants: map[string]int{}, logVMs: map[string]int{}}
	for _, m := range cfg.Media {
		if m == nil || m.MediaKey == "" || s.media[m.MediaKey] != nil {
			return nil, biz.ErrInvalid
		}
		s.media[m.MediaKey] = proto.Clone(m).(*pb.MediaReference)
	}
	return s, nil
}
func rpcError(e error) error {
	if e == nil {
		return nil
	}
	code := codes.Unavailable
	switch {
	case errors.Is(e, biz.ErrDenied):
		code = codes.PermissionDenied
	case errors.Is(e, biz.ErrInvalid), errors.Is(e, pb.ErrInvalidRequest):
		code = codes.InvalidArgument
	case errors.Is(e, biz.ErrNotFound):
		code = codes.NotFound
	case errors.Is(e, biz.ErrIdempotency):
		code = codes.AlreadyExists
	case errors.Is(e, biz.ErrVersion), errors.Is(e, biz.ErrBusy), errors.Is(e, biz.ErrOwnership), errors.Is(e, biz.ErrChanged), errors.Is(e, biz.ErrConsole):
		code = codes.FailedPrecondition
	case errors.Is(e, biz.ErrLogTarget):
		code = codes.FailedPrecondition
	case errors.Is(e, biz.ErrLogLimit):
		code = codes.ResourceExhausted
	}
	if code == codes.Unavailable {
		return status.Error(code, "VM_DEPENDENCY_NOT_READY")
	}
	return status.Error(code, e.Error())
}
func (s *Service) ResolveCreateSpec(ctx context.Context, r *pb.ResolveCreateSpecRequest) (*pb.ResolveCreateSpecResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	if r.VmContractVersion != pb.ContractVersion || pb.ValidateUserIntent(r.Data) != nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	intent := proto.Clone(r.Data).(*pb.CreateVMData)
	intent.Dns, _ = pb.NormalizeDNSSelection(intent.Dns)
	dns, e := s.resolveDNS(intent.Dns)
	if e != nil {
		return nil, rpcError(e)
	}
	ns, e := s.environment.ResolveNamespace(ctx, r.TenantId, r.Namespace)
	if e != nil {
		return nil, rpcError(e)
	}
	if r.NamespaceUid != "" && r.NamespaceUid != ns.UID {
		return nil, rpcError(biz.ErrChanged)
	}
	selection, e := s.compute.ResolveComputePreset(ctx, &compute.ResolveComputePresetRequest{TenantId: r.TenantId, PresetId: r.Data.PresetId, Usage: compute.Usage_VIRTUAL_MACHINE})
	if e != nil {
		return nil, e
	}
	media := s.media[r.Data.MediaKey]
	if media == nil {
		return nil, rpcError(biz.ErrDependency)
	}
	media = proto.Clone(media).(*pb.MediaReference)
	if e = s.native.ValidateSource(ctx, domainSource(media)); e != nil {
		return nil, rpcError(e)
	}
	if e = s.environment.ValidateStorage(ctx, s.config.StorageClass, s.config.StorageClassUID); e != nil {
		return nil, rpcError(e)
	}
	selectionNetwork, e := s.environment.ValidateNetwork(ctx, r.TenantId, s.config.ClusterID, ns.Name, biz.NetworkSelection{VPCID: r.Data.Network.VpcId, SubnetID: r.Data.Network.SubnetId})
	if e != nil {
		return nil, rpcError(e)
	}
	spec := &pb.CreateSpec{Intent: intent, Compute: selection, ClusterId: s.config.ClusterID, Namespace: ns.Name, NamespaceUid: ns.UID, ServiceAccount: s.config.ServiceAccount, Storage: &pb.StoragePolicy{StorageClass: s.config.StorageClass, StorageClassUid: s.config.StorageClassUID, VolumeMode: "Block", AccessModes: []string{"ReadWriteOnce"}, BackendStorageClass: s.config.BackendStorageClass, MediaStorageClass: s.config.MediaStorageClass}, Media: media, Network: &pb.FrozenNetwork{VpcId: selectionNetwork.VPCID, SubnetId: selectionNetwork.SubnetID, NadName: selectionNetwork.NADName, NadUid: selectionNetwork.NADUID, NadConfigDigest: selectionNetwork.NADDigest}, Sockets: 1, Cores: uint32((selection.CpuMillis + 999) / 1000), Threads: 1, BootDevice: "SYSTEM_DISK", Dns: dns}
	if media.Kind == "INSTALL_ISO" {
		spec.BootDevice = "INSTALL_MEDIA"
	}
	if media.DriverMediaKey != "" {
		driver := s.media[media.DriverMediaKey]
		if driver == nil {
			return nil, rpcError(biz.ErrDependency)
		}
		spec.DriverMedia = proto.Clone(driver).(*pb.MediaReference)
		if e = s.native.ValidateSource(ctx, domainSource(driver)); e != nil {
			return nil, rpcError(e)
		}
	}
	digest, e := pb.CreateSpecDigest(spec)
	if e != nil {
		return nil, rpcError(e)
	}
	return &pb.ResolveCreateSpecResponse{CreateSpec: spec, VmContractVersion: pb.ContractVersion, SelectionDigest: selection.SelectionDigest, VmCreateDigest: digest}, nil
}
func domainSource(m *pb.MediaReference) biz.Source {
	if m == nil {
		return biz.Source{}
	}
	return biz.Source{Key: m.MediaKey, Kind: m.Kind, ClusterID: m.ClusterId, Namespace: m.Namespace, PVCName: m.PvcName, PVCUID: m.PvcUid, Checksum: m.Checksum, VolumeMode: m.VolumeMode, ContentType: m.ContentType, VirtualBytes: m.VirtualCapacityBytes, RequestedBytes: m.RequestedBytes, OSFamily: m.OsFamily, OSVersion: m.OsVersion, Architecture: m.Architecture, Firmware: m.Firmware, RootBus: m.RootBus, DataBus: m.DataBus, NICModel: m.NicModel, PersistentTPM: m.PersistentTpm, PersistentUEFI: m.PersistentUefi}
}
func domainSpec(vmid string, p *pb.CreateSpec) (biz.Spec, error) {
	raw, e := pb.CanonicalCreateSpec(p)
	if e != nil {
		return biz.Spec{}, e
	}
	digest, e := pb.CreateSpecDigest(p)
	if e != nil {
		return biz.Spec{}, e
	}
	request, e := pb.RequestDigest(p.Intent)
	if e != nil {
		return biz.Spec{}, e
	}
	s := biz.Spec{Canonical: raw, Digest: digest, RequestDigest: request, Name: p.Intent.Name, PresetID: p.Compute.PresetId, SelectionDigest: p.Compute.SelectionDigest, ClusterID: p.ClusterId, Namespace: p.Namespace, NamespaceUID: p.NamespaceUid, ServiceAccount: p.ServiceAccount, StorageClass: p.Storage.StorageClass, StorageClassUID: p.Storage.StorageClassUid, BackendStorageClass: p.Storage.BackendStorageClass, MediaStorageClass: p.Storage.MediaStorageClass, CPUMillis: p.Compute.CpuMillis, MemoryMiB: p.Compute.MemoryMib, Cores: p.Cores, Media: domainSource(p.Media), Network: biz.NetworkSelection{VPCID: p.Network.VpcId, SubnetID: p.Network.SubnetId, NADName: p.Network.NadName, NADUID: p.Network.NadUid, NADDigest: p.Network.NadConfigDigest}, BootDevice: p.BootDevice, StartAfterCreate: p.Intent.GetStartAfterCreate()}
	if p.DriverMedia != nil {
		d := domainSource(p.DriverMedia)
		s.Driver = &d
	}
	if p.Dns != nil {
		s.DNS = &biz.DNS{Mode: p.Dns.Mode, Nameservers: append([]string(nil), p.Dns.Nameservers...)}
	}
	if g := p.Intent.GuestInitialization; g != nil {
		s.GuestInitialization = &biz.GuestInitialization{Method: g.Method, Username: g.Username, SSHPublicKey: g.SshPublicKey, PasswordSet: g.PasswordSet, CredentialIntentDigest: g.CredentialIntentDigest}
	}
	s.InitialDisks = []biz.Disk{biz.NewInitialDisk(vmid, "system", "system", "system", p.Intent.SystemDiskSizeGb)}
	for _, d := range p.Intent.DataDisks {
		s.InitialDisks = append(s.InitialDisks, biz.NewInitialDisk(vmid, d.ClientDiskKey, d.Name, "data", d.SizeGb))
	}
	for i := range s.InitialDisks {
		s.InitialDisks[i].DV.Namespace = p.Namespace
	}
	return s, nil
}
func (s *Service) AcceptCreate(ctx context.Context, r *pb.AcceptCreateRequest) (*pb.Acceptance, error) {
	if r == nil || r.Context == nil || r.VmContractVersion != pb.ContractVersion {
		return nil, rpcError(biz.ErrInvalid)
	}
	c := r.Context
	if e := biz.RequireTenant(ctx, c.TenantId); e != nil {
		return nil, rpcError(e)
	}
	spec, e := domainSpec(c.ResourceId, r.CreateSpec)
	if e != nil {
		return nil, rpcError(e)
	}
	if spec.Digest != r.VmCreateDigest || spec.RequestDigest != c.RequestDigest {
		return nil, rpcError(biz.ErrIdempotency)
	}
	if e = pb.ValidateGuestCredentials(r.CreateSpec, r.GuestCredentials); e != nil {
		return nil, rpcError(e)
	}
	password := ""
	if r.GuestCredentials != nil {
		password = r.GuestCredentials.Password
	}
	o, e := s.repo.AcceptCreate(ctx, biz.Acceptance{TenantID: c.TenantId, VMID: c.ResourceId, OperationID: c.OperationId, RequestDigest: c.RequestDigest, Spec: spec, Password: password})
	if e != nil {
		return nil, rpcError(e)
	}
	return &pb.Acceptance{OperationId: o.ID, ResourceId: o.VMID, Accepted: true}, nil
}
func (s *Service) AcceptDelete(ctx context.Context, r *pb.AcceptDeleteRequest) (*pb.Acceptance, error) {
	if r == nil || r.Context == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	c := r.Context
	if e := biz.RequireTenant(ctx, c.TenantId); e != nil {
		return nil, rpcError(e)
	}
	d, e := pb.DeleteRequestDigest(r)
	if e != nil {
		return nil, rpcError(e)
	}
	if d != c.RequestDigest {
		return nil, rpcError(biz.ErrIdempotency)
	}
	o, e := s.repo.AcceptDelete(ctx, biz.Acceptance{TenantID: c.TenantId, VMID: c.ResourceId, OperationID: c.OperationId, RequestDigest: c.RequestDigest, CreateOperationID: r.CreateOperationId, ExpectedVersion: r.ExpectedVersion})
	if e != nil {
		return nil, rpcError(e)
	}
	return &pb.Acceptance{OperationId: o.ID, ResourceId: o.VMID, Accepted: true}, nil
}
func (s *Service) GetVM(ctx context.Context, r *pb.GetVMRequest) (*pb.VirtualMachine, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	v, e := s.repo.Get(ctx, r.TenantId, r.VmId)
	if e != nil {
		return nil, rpcError(e)
	}
	return s.project(ctx, v)
}
func wireVM(v biz.VM) (*pb.VirtualMachine, error) {
	spec, e := pb.DecodeCreateSpec(v.Spec.Canonical)
	if e != nil {
		return nil, rpcError(e)
	}
	out := &pb.VirtualMachine{Id: v.ID, Name: v.Name, Version: v.Version, OsFamily: v.Spec.Media.OSFamily, OsVersion: v.Spec.Media.OSVersion, FrozenSpec: spec, DesiredState: v.Desired, ObservedState: v.Observed, Reason: v.Reason, ObservedAt: observed(v.ObservedAt), LastOperationId: v.LastOperationID, Closing: v.Closing, BootDevice: v.Spec.BootDevice}
	for _, d := range v.Disks {
		if !d.Tombstone {
			out.Disks = append(out.Disks, &pb.Disk{DiskId: d.ID, Role: d.Role, Name: d.Name, SizeGb: d.SizeGB, RequestedBytes: d.RequestedBytes, ObservedCapacityBytes: d.ObservedCapacityBytes, VolumeName: d.VolumeName, Serial: d.Serial, Attached: d.Attached, PreparationState: d.Preparation, Reason: d.Reason})
		}
	}
	for _, a := range v.Addresses {
		out.Addresses = append(out.Addresses, &pb.CurrentAddress{NicName: a.NIC, Address: a.Address, ObservedAt: observed(v.ObservedAt)})
	}
	wireMedia(v, out)
	return out, nil
}
func observed(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func wireOp(o biz.Operation) *pb.Operation {
	return &pb.Operation{Id: o.ID, VmId: o.VMID, Action: o.Action, IntentVersion: o.IntentVersion, State: o.State, Reason: o.Reason, ObservedAt: observed(o.ObservedAt)}
}
func (s *Service) ListVMs(ctx context.Context, r *pb.ListVMsRequest) (*pb.ListVMsResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	all, total, e := s.repo.List(ctx, r.TenantId, r.Page, r.PageSize)
	if e != nil {
		return nil, rpcError(e)
	}
	out := &pb.ListVMsResponse{Total: total}
	for _, v := range all {
		x, e := s.project(ctx, v)
		if e != nil {
			return nil, e
		}
		out.Items = append(out.Items, x)
	}
	return out, nil
}
func (s *Service) GetOperation(ctx context.Context, r *pb.GetOperationRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	o, e := s.repo.GetOperation(ctx, r.TenantId, r.VmId, r.OperationId)
	if e != nil {
		return nil, rpcError(e)
	}
	return wireOp(o), nil
}
func (s *Service) LookupCommandReplay(ctx context.Context, r *pb.LookupCommandReplayRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	if !validAction(r.Action) {
		return nil, rpcError(biz.ErrInvalid)
	}
	o, e := s.repo.LookupReplay(ctx, biz.Command{TenantID: r.TenantId, VMID: r.VmId, Action: r.Action, Key: r.IdempotencyKey, RequestDigest: pb.Digest(r.CanonicalIntent)})
	if e != nil {
		return nil, rpcError(e)
	}
	return wireOp(o), nil
}
func validAction(a string) bool {
	switch a {
	case "start", "stop", "restart", "force-stop", "create-disk", "attach-disk", "detach-disk", "delete-disk", "set-media", "eject-media", "set-boot", "create-snapshot", "delete-snapshot", "restore-snapshot":
		return true
	}
	return false
}
func (s *Service) command(ctx context.Context, m proto.Message, action string) (*pb.Operation, error) {
	x := m.ProtoReflect()
	tenant := x.Get(x.Descriptor().Fields().ByName("tenant_id")).String()
	if e := biz.RequireTenant(ctx, tenant); e != nil {
		return nil, rpcError(e)
	}
	raw, e := pb.CanonicalCommandIntent(m)
	if e != nil {
		return nil, rpcError(e)
	}
	c := biz.Command{TenantID: tenant, VMID: x.Get(x.Descriptor().Fields().ByName("vm_id")).String(), Action: action, Key: x.Get(x.Descriptor().Fields().ByName("idempotency_key")).String(), ExpectedVersion: x.Get(x.Descriptor().Fields().ByName("expected_version")).Int(), Intent: raw, RequestDigest: pb.Digest(raw)}
	if !biz.UUID(c.VMID) || c.Key == "" || len(c.Key) > 256 {
		return nil, rpcError(biz.ErrInvalid)
	}
	if o, e := s.repo.LookupReplay(ctx, c); !errors.Is(e, biz.ErrNotFound) {
		if e != nil {
			return nil, rpcError(e)
		}
		return wireOp(o), nil
	}
	if action == "force-stop" && !m.(*pb.ForceStopRequest).DestructiveConfirmation {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e = s.validateSnapshotCommand(ctx, c); e != nil {
		return nil, rpcError(e)
	}
	if action == "create-disk" || action == "attach-disk" || action == "detach-disk" || action == "delete-disk" {
		v, e := s.repo.Get(ctx, c.TenantID, c.VMID)
		if e != nil {
			return nil, rpcError(e)
		}
		if e = biz.ValidateDiskCommand(v, action, raw); e != nil {
			return nil, rpcError(e)
		}
		if action != "create-disk" {
			intent, _ := biz.ParseDiskIntent(raw)
			i, _ := biz.FindDisk(v, intent.DiskID)
			n, ok := s.native.(biz.DiskNativePort)
			if !ok {
				return nil, rpcError(biz.ErrDependency)
			}
			if action == "delete-disk" {
				clear, e := n.DiskUnreferenced(ctx, v, v.Disks[i])
				if e != nil {
					return nil, rpcError(e)
				}
				if !clear {
					return nil, rpcError(biz.ErrBusy)
				}
			} else {
				if e = n.ValidateDiskAttachment(ctx, v, v.Disks[i], action == "attach-disk"); e != nil {
					return nil, rpcError(e)
				}
				observed, e := s.native.Observe(ctx, v)
				if e != nil {
					return nil, rpcError(e)
				}
				if action == "detach-disk" && observed.Running && !intent.GuestPrepared {
					return nil, rpcError(biz.ErrInvalid)
				}
			}
		}
	}
	if action == "start" || action == "restart" {
		v, e := s.repo.Get(ctx, c.TenantID, c.VMID)
		if e != nil {
			return nil, rpcError(e)
		}
		ns, e := s.environment.ResolveNamespace(ctx, c.TenantID, v.Spec.Namespace)
		if e != nil {
			return nil, rpcError(e)
		}
		if ns.UID != v.Spec.NamespaceUID {
			return nil, rpcError(biz.ErrChanged)
		}
		if biz.NeedsBackendState(v.Spec) {
			native, ok := s.native.(biz.BackendNativePort)
			if !ok {
				return nil, rpcError(biz.ErrDependency)
			}
			if e = native.ValidateBackendConfiguration(ctx, v.Spec); e != nil {
				return nil, rpcError(e)
			}
		}
		network, e := s.environment.ValidateNetwork(ctx, c.TenantID, v.Spec.ClusterID, v.Spec.Namespace, v.Spec.Network)
		if e != nil {
			return nil, rpcError(e)
		}
		if network != v.Spec.Network {
			return nil, rpcError(biz.ErrChanged)
		}
	}
	o, e := s.repo.AcceptCommand(ctx, c)
	if e != nil {
		return nil, rpcError(e)
	}
	return wireOp(o), nil
}
func (s *Service) Start(ctx context.Context, r *pb.ChangeVMRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "start")
}
func (s *Service) Stop(ctx context.Context, r *pb.ChangeVMRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "stop")
}
func (s *Service) Restart(ctx context.Context, r *pb.ChangeVMRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "restart")
}
func (s *Service) ForceStop(ctx context.Context, r *pb.ForceStopRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "force-stop")
}
func (s *Service) ListDisks(ctx context.Context, r *pb.GetVMRequest) (*pb.ListDisksResponse, error) {
	v, e := s.GetVM(ctx, r)
	if e != nil {
		return nil, e
	}
	return &pb.ListDisksResponse{Items: v.Disks}, nil
}
func (s *Service) ListMedia(ctx context.Context, r *pb.ListMediaRequest) (*pb.ListMediaResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	out := &pb.ListMediaResponse{}
	for _, m := range s.config.Media {
		out.Items = append(out.Items, proto.Clone(m).(*pb.MediaReference))
	}
	return out, nil
}
func (s *Service) resolveConsole(ctx context.Context, tenant, id string, version int64) (*console.VMConsoleBinding, error) {
	v, e := s.repo.Get(ctx, tenant, id)
	if e != nil {
		return nil, e
	}
	if v.Closing || v.Version != version || version < 1 {
		return nil, biz.ErrConsole
	}
	ns, e := s.environment.ResolveNamespace(ctx, tenant, v.Spec.Namespace)
	if e != nil {
		return nil, e
	}
	if ns.UID != v.Spec.NamespaceUID {
		return nil, biz.ErrConsole
	}
	if v.LastOperationID != "" {
		o, e := s.repo.GetOperation(ctx, tenant, id, v.LastOperationID)
		if e != nil {
			return nil, e
		}
		if o.State != "succeeded" && o.State != "failed" && (o.Action == "stop" || o.Action == "force-stop" || o.Action == "restart" || o.Action == "restore-snapshot" || o.Action == biz.DeleteAction) {
			return nil, biz.ErrConsole
		}
	}
	var x biz.Observation
	if native, ok := s.native.(biz.ConsoleNativePort); ok {
		x, e = native.ObserveConsole(ctx, v)
	} else {
		x, e = s.native.Observe(ctx, v)
	}
	if e != nil {
		return nil, e
	}
	if !x.Running || x.VMI == nil || x.VM.UID != v.Native.UID {
		return nil, biz.ErrConsole
	}
	current, e := s.repo.Get(ctx, tenant, id)
	if e != nil {
		return nil, e
	}
	if current.Closing || current.Version != version || current.Native.UID != v.Native.UID {
		return nil, biz.ErrConsole
	}
	return &console.VMConsoleBinding{TenantId: tenant, VmId: id, Version: version, ClusterId: v.Spec.ClusterID, Namespace: v.Spec.Namespace, NamespaceUid: v.Spec.NamespaceUID, VmName: v.Native.Name, VmUid: v.Native.UID, VmiName: x.VMI.Name, VmiUid: x.VMI.UID}, nil
}
func (s *Service) ResolveConsoleTarget(ctx context.Context, r *pb.ResolveConsoleTargetRequest) (*console.VMConsoleBinding, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	v, e := s.resolveConsole(ctx, r.TenantId, r.VmId, r.ExpectedVersion)
	return v, rpcError(e)
}
func (s *Service) ValidateConsoleSession(ctx context.Context, r *console.VMConsoleBinding) (*console.ConsoleSessionValidation, error) {
	if r == nil || !biz.UUID(r.TenantId) || !biz.UUID(r.VmId) || len(r.ProtoReflect().GetUnknown()) != 0 {
		return nil, rpcError(biz.ErrConsole)
	}
	for _, v := range []string{r.ClusterId, r.Namespace, r.NamespaceUid, r.VmName, r.VmUid, r.VmiName, r.VmiUid} {
		if strings.TrimSpace(v) == "" {
			return nil, rpcError(biz.ErrConsole)
		}
	}
	current, e := s.resolveConsole(ctx, r.TenantId, r.VmId, r.Version)
	if e != nil {
		return nil, rpcError(e)
	}
	if !proto.Equal(current, r) {
		return nil, rpcError(biz.ErrConsole)
	}
	return &console.ConsoleSessionValidation{Valid: true}, nil
}

func (s *Service) acquireLog(v biz.VM) (func(), error) {
	limits := s.config.LogLimits.Defaults()
	key := v.TenantID + ":" + v.ID
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logTotal >= limits.ProcessConnections || s.logTenants[v.TenantID] >= limits.TenantConnections || s.logVMs[key] >= limits.InstanceConnections {
		return nil, biz.ErrLogLimit
	}
	s.logTotal++
	s.logTenants[v.TenantID]++
	s.logVMs[key]++
	return func() {
		s.logMu.Lock()
		defer s.logMu.Unlock()
		s.logTotal--
		s.logTenants[v.TenantID]--
		s.logVMs[key]--
		if s.logTenants[v.TenantID] == 0 {
			delete(s.logTenants, v.TenantID)
		}
		if s.logVMs[key] == 0 {
			delete(s.logVMs, key)
		}
	}, nil
}

// StreamVMLogs serves the guest serial console of the running launcher. The
// caller's lifetime owns the stream; frames are only released after READY.
func (s *Service) StreamVMLogs(r *pb.StreamVMLogsRequest, stream grpc.ServerStreamingServer[pb.VMLogFrame]) error {
	if r == nil || r.TailLines < 1 || r.TailLines > 1000 {
		return rpcError(biz.ErrInvalid)
	}
	ctx := stream.Context()
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return rpcError(e)
	}
	if !biz.UUID(r.VmId) {
		return rpcError(biz.ErrInvalid)
	}
	v, e := s.repo.Get(ctx, r.TenantId, r.VmId)
	if e != nil {
		return rpcError(e)
	}
	if v.TenantID != r.TenantId || v.ID != r.VmId {
		return rpcError(biz.ErrDenied)
	}
	if v.Spec.ClusterID != s.config.ClusterID || s.logs == nil {
		return rpcError(biz.ErrLogTarget)
	}
	release, e := s.acquireLog(v)
	if e != nil {
		return rpcError(e)
	}
	defer release()
	var x biz.Observation
	if native, ok := s.native.(biz.ConsoleNativePort); ok {
		x, e = native.ObserveConsole(ctx, v)
	} else {
		x, e = s.native.Observe(ctx, v)
	}
	if e != nil {
		return rpcError(e)
	}
	if !x.Running || x.VMI == nil || x.VM.UID != v.Native.UID {
		return rpcError(biz.ErrLogTarget)
	}
	limits := s.config.LogLimits.Defaults()
	call, cancel := context.WithTimeout(ctx, time.Duration(limits.LifetimeSeconds)*time.Second)
	defer cancel()
	return s.logs.StreamLogs(call, v, x, r.TailLines, r.Follow, func(frame biz.LogFrame) error {
		if len(frame.Data) > limits.FrameBytes {
			return biz.ErrInvalid
		}
		out := &pb.VMLogFrame{Kind: pb.VMLogFrame_Kind(pb.VMLogFrame_Kind_value[frame.Kind]), Data: frame.Data, Reason: frame.Reason}
		done := make(chan error, 1)
		go func() { done <- stream.Send(out) }()
		timer := time.NewTimer(time.Duration(limits.WriteTimeoutSeconds) * time.Second)
		defer timer.Stop()
		select {
		case sendErr := <-done:
			return sendErr
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-timer.C:
			return status.Error(codes.DeadlineExceeded, "VM_LOG_WRITE_TIMEOUT")
		}
	})
}
