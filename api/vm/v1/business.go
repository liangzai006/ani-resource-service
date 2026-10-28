package vmv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const ContractVersion uint32 = 1
const BytesPerGB int64 = 1000000000

var ErrInvalidRequest = errors.New("VM_INVALID_REQUEST")
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var dnsPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var vpcIDPattern = regexp.MustCompile(`^vpc_[0-9a-f]{32}$`)
var subnetIDPattern = regexp.MustCompile(`^subnet_[0-9a-f]{32}$`)
var guestUsernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

const CloudInitNoCloud = "CLOUD_INIT_NOCLOUD"

const DNSPlatformDefault = "PLATFORM_DEFAULT"
const DNSCustom = "CUSTOM"

// NormalizeDNSServers accepts one to three IP literals, renders a canonical IP
// spelling and removes duplicates without changing the resolver preference order.
// Hostnames, CIDRs, ports, zones and whitespace are not DNS server addresses.
func NormalizeDNSServers(servers []string) ([]string, error) {
	if len(servers) < 1 || len(servers) > 3 {
		return nil, ErrInvalidRequest
	}
	out := make([]string, 0, len(servers))
	seen := map[string]bool{}
	for _, server := range servers {
		ip, e := netip.ParseAddr(server)
		if e != nil || ip.Zone() != "" {
			return nil, ErrInvalidRequest
		}
		normalized := ip.Unmap().String()
		if !seen[normalized] {
			out = append(out, normalized)
			seen[normalized] = true
		}
	}
	return out, nil
}

// NormalizeDNSSelection keeps platform-default intent absent so clients which
// predate DNS selection retain their accepted request digest. FrozenDNS, unlike
// this selector, always records the actual addresses used by a new resolution.
func NormalizeDNSSelection(d *DNSSelection) (*DNSSelection, error) {
	if d == nil {
		return nil, nil
	}
	if hasUnknown(d.ProtoReflect()) {
		return nil, ErrInvalidRequest
	}
	if d.Mode == "" || d.Mode == DNSPlatformDefault {
		if len(d.Nameservers) != 0 {
			return nil, ErrInvalidRequest
		}
		return nil, nil
	}
	if d.Mode != DNSCustom {
		return nil, ErrInvalidRequest
	}
	servers, e := NormalizeDNSServers(d.Nameservers)
	if e != nil {
		return nil, e
	}
	return &DNSSelection{Mode: DNSCustom, Nameservers: servers}, nil
}

func validateFrozenDNS(s *CreateSpec) error {
	selector, e := NormalizeDNSSelection(s.Intent.Dns)
	if e != nil {
		return e
	}
	if s.Dns == nil {
		// Legacy frozen specs did not include DNS. Reading and replaying them
		// must neither require platform defaults nor change their digest.
		if selector == nil {
			return nil
		}
		return ErrInvalidRequest
	}
	servers, e := NormalizeDNSServers(s.Dns.Nameservers)
	if e != nil || !slices.Equal(servers, s.Dns.Nameservers) {
		return ErrInvalidRequest
	}
	if selector == nil {
		if s.Dns.Mode != DNSPlatformDefault {
			return ErrInvalidRequest
		}
	} else if s.Dns.Mode != DNSCustom || !slices.Equal(selector.Nameservers, s.Dns.Nameservers) {
		return ErrInvalidRequest
	}
	return nil
}

func validateGuestInitialization(g *GuestInitialization) error {
	if g == nil {
		return nil
	}
	if g.Method != CloudInitNoCloud || !guestUsernamePattern.MatchString(g.Username) || g.Username == "root" || hasUnknown(g.ProtoReflect()) || (!g.PasswordSet && g.SshPublicKey == "") {
		return ErrInvalidRequest
	}
	if (g.PasswordSet && !digestPattern.MatchString(g.CredentialIntentDigest)) || (!g.PasswordSet && g.CredentialIntentDigest != "") {
		return ErrInvalidRequest
	}
	if g.SshPublicKey != "" {
		if len(g.SshPublicKey) > 16384 || strings.ContainsAny(g.SshPublicKey, "\r\n\x00") {
			return ErrInvalidRequest
		}
		_, _, options, rest, e := ssh.ParseAuthorizedKey([]byte(g.SshPublicKey))
		if e != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
			return ErrInvalidRequest
		}
	}
	return nil
}

// ValidateGuestCredentials accepts a write-only password, never a public key pair.
func ValidateGuestCredentials(s *CreateSpec, c *GuestCredentials) error {
	if s == nil || ValidateCreateSpec(s) != nil {
		return ErrInvalidRequest
	}
	g := s.Intent.GuestInitialization
	if c != nil && hasUnknown(c.ProtoReflect()) {
		return ErrInvalidRequest
	}
	if g == nil {
		if c != nil {
			return ErrInvalidRequest
		}
		return nil
	}
	password := ""
	if c != nil {
		password = c.Password
	}
	if !g.PasswordSet {
		if password != "" {
			return ErrInvalidRequest
		}
		return nil
	}
	if len(password) < 8 || len(password) > 128 || !utf8.ValidString(password) || strings.ContainsAny(password, "\x00\r\n") {
		return ErrInvalidRequest
	}
	return nil
}

func validUUID(s string) bool {
	u, e := uuid.Parse(s)
	return e == nil && u != uuid.Nil && u.String() == s
}
func validName(s string) bool { return len(s) > 0 && len(s) <= 63 && dnsPattern.MatchString(s) }
func validSize(n int64) bool  { return n > 0 && n <= math.MaxInt64/BytesPerGB }
func hasUnknown(m protoreflect.Message) bool {
	if len(m.GetUnknown()) != 0 {
		return true
	}
	bad := false
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if f.IsMap() {
			return true
		}
		if f.Kind() == protoreflect.MessageKind {
			if f.IsList() {
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					bad = bad || hasUnknown(l.Get(i).Message())
				}
			} else {
				bad = bad || hasUnknown(v.Message())
			}
		}
		return !bad
	})
	return bad
}

func ValidateUserIntent(d *CreateVMData) error {
	if d == nil || hasUnknown(d.ProtoReflect()) || strings.TrimSpace(d.Name) == "" || utf8.RuneCountInString(d.Name) > 128 || !validUUID(d.PresetId) || d.MediaKey == "" || !validSize(d.SystemDiskSizeGb) || d.StartAfterCreate == nil || d.Network == nil || !vpcIDPattern.MatchString(d.Network.VpcId) || !subnetIDPattern.MatchString(d.Network.SubnetId) {
		return ErrInvalidRequest
	}
	if d.SourceKind != "DISK_IMAGE" && d.SourceKind != "INSTALL_ISO" {
		return ErrInvalidRequest
	}
	if validateGuestInitialization(d.GuestInitialization) != nil || (d.GuestInitialization != nil && d.SourceKind != "DISK_IMAGE") {
		return ErrInvalidRequest
	}
	if _, e := NormalizeDNSSelection(d.Dns); e != nil {
		return e
	}
	seen := map[string]bool{}
	for _, disk := range d.DataDisks {
		if disk == nil || strings.TrimSpace(disk.ClientDiskKey) == "" || len(disk.ClientDiskKey) > 128 || seen[disk.ClientDiskKey] || strings.TrimSpace(disk.Name) == "" || utf8.RuneCountInString(disk.Name) > 128 || !validSize(disk.SizeGb) {
			return ErrInvalidRequest
		}
		seen[disk.ClientDiskKey] = true
	}
	return nil
}

func validateMedia(m *MediaReference) error {
	if m == nil || m.MediaKey == "" || !validUUID(m.ClusterId) || !validName(m.Namespace) || !validName(m.PvcName) || m.PvcUid == "" || !digestPattern.MatchString(m.Checksum) || m.VirtualCapacityBytes <= 0 || m.RequestedBytes <= 0 || m.OsVersion == "" || m.Architecture != "amd64" {
		return ErrInvalidRequest
	}
	if m.OsFamily != "linux" && m.OsFamily != "windows" {
		return ErrInvalidRequest
	}
	if m.Kind != "DISK_IMAGE" && m.Kind != "INSTALL_ISO" && m.Kind != "DRIVER_ISO" {
		return ErrInvalidRequest
	}
	if m.VolumeMode != "Block" && m.VolumeMode != "Filesystem" {
		return ErrInvalidRequest
	}
	if m.ContentType != "kubevirt" {
		return ErrInvalidRequest
	}
	if m.Firmware != "bios" && m.Firmware != "uefi" {
		return ErrInvalidRequest
	}
	if m.RootBus != "virtio" && m.RootBus != "scsi" && m.RootBus != "sata" {
		return ErrInvalidRequest
	}
	if m.DataBus != "scsi" || (m.NicModel != "virtio" && m.NicModel != "e1000" && m.NicModel != "e1000e") {
		return ErrInvalidRequest
	}
	if m.InitializationCapability != "" && m.InitializationCapability != "NONE" && m.InitializationCapability != CloudInitNoCloud {
		return ErrInvalidRequest
	}
	if m.InitializationVerified && (m.InitializationCapability != CloudInitNoCloud || m.Kind != "DISK_IMAGE" || m.OsFamily != "linux") {
		return ErrInvalidRequest
	}
	return nil
}

func ValidateCreateSpec(s *CreateSpec) error {
	if s == nil || hasUnknown(s.ProtoReflect()) || ValidateUserIntent(s.Intent) != nil || compute.ValidateSelection(s.Compute) != nil || !validUUID(s.ClusterId) || !validName(s.Namespace) || s.NamespaceUid == "" || !validName(s.ServiceAccount) || s.Storage == nil || s.Network == nil || validateMedia(s.Media) != nil {
		return ErrInvalidRequest
	}
	if e := validateFrozenDNS(s); e != nil {
		return e
	}
	p := s.Storage
	if !validName(p.StorageClass) || p.StorageClassUid == "" || p.VolumeMode != "Block" || len(p.AccessModes) != 1 || p.AccessModes[0] != "ReadWriteOnce" {
		return ErrInvalidRequest
	}
	if s.Compute.PresetId != s.Intent.PresetId || s.Media.MediaKey != s.Intent.MediaKey || s.Media.Kind != s.Intent.SourceKind || s.Media.ClusterId != s.ClusterId || s.Sockets != 1 || s.Threads != 1 || s.Compute.CpuMillis > int64(math.MaxUint32)*1000 || s.Cores != uint32((s.Compute.CpuMillis+999)/1000) {
		return ErrInvalidRequest
	}
	n := s.Network
	if n.VpcId != s.Intent.Network.VpcId || n.SubnetId != s.Intent.Network.SubnetId || !validName(n.NadName) || n.NadUid == "" || !digestPattern.MatchString(n.NadConfigDigest) {
		return ErrInvalidRequest
	}
	if s.Media.Kind == "DISK_IMAGE" && (s.Intent.SystemDiskSizeGb*BytesPerGB < s.Media.VirtualCapacityBytes || (s.Media.VolumeMode == "Block" && s.Intent.SystemDiskSizeGb*BytesPerGB < s.Media.RequestedBytes)) {
		return ErrInvalidRequest
	}
	if (s.Intent.SourceKind == "DISK_IMAGE" && s.BootDevice != "SYSTEM_DISK") || (s.Intent.SourceKind == "INSTALL_ISO" && s.BootDevice != "INSTALL_MEDIA") {
		return ErrInvalidRequest
	}
	if s.Intent.GuestInitialization != nil && (s.Media.OsFamily != "linux" || s.Media.Kind != "DISK_IMAGE" || s.Media.InitializationCapability != CloudInitNoCloud || !s.Media.InitializationVerified) {
		return ErrInvalidRequest
	}
	if s.DriverMedia != nil {
		if validateMedia(s.DriverMedia) != nil || s.DriverMedia.Kind != "DRIVER_ISO" || s.DriverMedia.MediaKey != s.Media.DriverMediaKey || s.DriverMedia.ClusterId != s.ClusterId || s.DriverMedia.OsFamily != s.Media.OsFamily {
			return ErrInvalidRequest
		}
	} else if s.Media.DriverMediaKey != "" {
		return ErrInvalidRequest
	}
	if (s.Media.PersistentTpm || s.Media.PersistentUefi) && (!validName(p.BackendStorageClass) || s.Media.Firmware != "uefi") {
		return ErrInvalidRequest
	}
	return nil
}

// Version 1 has a closed field projection. Never add a new field to this list
// when evolving descriptors: an accepted payload must retain its original bytes.
var canonicalFields = map[protoreflect.FullName][]string{
	"vm.v1.CreateVMData":        {"name", "preset_id", "source_kind", "media_key", "system_disk_size_gb", "data_disks", "network", "start_after_create"},
	"vm.v1.InitialDataDisk":     {"client_disk_key", "name", "size_gb"},
	"vm.v1.NetworkSelection":    {"vpc_id", "subnet_id"},
	"vm.v1.CreateSpec":          {"intent", "compute", "cluster_id", "namespace", "namespace_uid", "service_account", "storage", "media", "driver_media", "network", "sockets", "cores", "threads", "boot_device"},
	"vm.v1.MediaReference":      {"media_key", "name", "kind", "cluster_id", "namespace", "pvc_name", "pvc_uid", "checksum", "volume_mode", "content_type", "virtual_capacity_bytes", "requested_bytes", "os_family", "os_version", "architecture", "firmware", "root_bus", "data_bus", "nic_model", "persistent_tpm", "persistent_uefi", "driver_media_key"},
	"vm.v1.StoragePolicy":       {"storage_class", "storage_class_uid", "volume_mode", "access_modes", "backend_storage_class", "media_storage_class"},
	"vm.v1.FrozenNetwork":       {"vpc_id", "subnet_id", "nad_name", "nad_uid", "nad_config_digest"},
	"vm.v1.GuestInitialization": {"method", "username", "ssh_public_key", "password_set", "credential_intent_digest"},
	"vm.v1.DNSSelection":        {"mode", "nameservers"},
	"vm.v1.FrozenDNS":           {"mode", "nameservers"},
}

// Optional extensions are omitted at their default value, preserving accepted
// v1 bytes for payloads which predate guest initialization or DNS selection.
var canonicalOptionalFields = map[protoreflect.FullName][]string{
	"vm.v1.CreateVMData":   {"guest_initialization", "dns"},
	"vm.v1.CreateSpec":     {"dns"},
	"vm.v1.MediaReference": {"initialization_capability", "initialization_verified"},
}

func closedProjection(m protoreflect.Message) (any, error) {
	if !m.IsValid() {
		return nil, nil
	}
	if m.Descriptor().FullName() == "compute.v1.ComputeSelection" {
		s := m.Interface().(*compute.ComputeSelection)
		b, e := compute.CanonicalSelection(s)
		if e != nil {
			return nil, e
		}
		var v map[string]any
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		if e = d.Decode(&v); e != nil {
			return nil, e
		}
		v["selection_digest"] = s.SelectionDigest
		return v, nil
	}
	fields, ok := canonicalFields[m.Descriptor().FullName()]
	if !ok {
		return nil, ErrInvalidRequest
	}
	out := map[string]any{}
	for _, name := range append(append([]string{}, fields...), canonicalOptionalFields[m.Descriptor().FullName()]...) {
		f := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if f == nil {
			return nil, ErrInvalidRequest
		}
		optional := false
		for _, n := range canonicalOptionalFields[m.Descriptor().FullName()] {
			optional = optional || n == name
		}
		if optional && !m.Has(f) {
			continue
		}
		v := m.Get(f)
		if f.IsList() {
			items := []any{}
			for i := 0; i < v.List().Len(); i++ {
				x, e := wireValue(f, v.List().Get(i))
				if e != nil {
					return nil, e
				}
				items = append(items, x)
			}
			out[name] = items
		} else {
			x, e := wireValue(f, v)
			if e != nil {
				return nil, e
			}
			out[name] = x
		}
	}
	return out, nil
}
func wireValue(f protoreflect.FieldDescriptor, v protoreflect.Value) (any, error) {
	if f.Kind() == protoreflect.MessageKind {
		return closedProjection(v.Message())
	}
	// Keep int64 as JSON strings, as required by protobuf JSON and its decoders.
	if f.Kind() == protoreflect.Int64Kind {
		return json.Number(v.String()).String(), nil
	}
	return v.Interface(), nil
}
func canonical(m proto.Message) ([]byte, error) {
	if m == nil || !m.ProtoReflect().IsValid() || hasUnknown(m.ProtoReflect()) {
		return nil, ErrInvalidRequest
	}
	v, e := closedProjection(m.ProtoReflect())
	if e != nil {
		return nil, e
	}
	return json.Marshal(v)
}
func CanonicalUserIntent(d *CreateVMData) ([]byte, error) {
	if e := ValidateUserIntent(d); e != nil {
		return nil, e
	}
	normalized := proto.Clone(d).(*CreateVMData)
	normalized.Dns, _ = NormalizeDNSSelection(d.Dns)
	return canonical(normalized)
}
func CanonicalCreateSpec(s *CreateSpec) ([]byte, error) {
	if e := ValidateCreateSpec(s); e != nil {
		return nil, e
	}
	normalized := proto.Clone(s).(*CreateSpec)
	normalized.Intent.Dns, _ = NormalizeDNSSelection(s.Intent.Dns)
	return canonical(normalized)
}
func DecodeCreateSpec(b []byte) (*CreateSpec, error) {
	s := new(CreateSpec)
	if e := protojson.Unmarshal(b, s); e != nil {
		return nil, e
	}
	if e := ValidateCreateSpec(s); e != nil {
		return nil, e
	}
	return s, nil
}
func Digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func CreateSpecDigest(s *CreateSpec) (string, error) {
	b, e := CanonicalCreateSpec(s)
	if e != nil {
		return "", e
	}
	return Digest(b), nil
}
func RequestDigest(d *CreateVMData) (string, error) {
	b, e := CanonicalUserIntent(d)
	if e != nil {
		return "", e
	}
	return Digest(b), nil
}

// DeleteRequestDigest excludes its own digest and binds the original create.
func DeleteRequestDigest(r *AcceptDeleteRequest) (string, error) {
	if r == nil || r.Context == nil || hasUnknown(r.ProtoReflect()) || !validUUID(r.Context.TenantId) || !validUUID(r.Context.ResourceId) || !validUUID(r.Context.OperationId) || !validUUID(r.CreateOperationId) || r.ExpectedVersion < 1 || r.VmContractVersion != ContractVersion {
		return "", ErrInvalidRequest
	}
	b, e := json.Marshal(map[string]any{"tenant_id": r.Context.TenantId, "resource_id": r.Context.ResourceId, "operation_id": r.Context.OperationId, "create_operation_id": r.CreateOperationId, "expected_version": r.ExpectedVersion, "vm_contract_version": r.VmContractVersion})
	if e != nil {
		return "", e
	}
	return Digest(b), nil
}

// CanonicalCommandIntent is shared by the direct acceptor and read-only replay.
// Parsing uses numbers without float conversion, preserving int64 intent.
func CanonicalCommandIntent(m proto.Message) ([]byte, error) {
	if m == nil || !m.ProtoReflect().IsValid() || hasUnknown(m.ProtoReflect()) {
		return nil, ErrInvalidRequest
	}
	switch m.(type) {
	case *ChangeVMRequest, *ForceStopRequest, *CreateDiskRequest, *DiskChangeRequest,
		*SetMediaRequest, *EjectMediaRequest, *SetBootDeviceRequest, *SnapshotCreateRequest,
		*SnapshotChangeRequest, *RestoreSnapshotRequest:
	default:
		return nil, ErrInvalidRequest
	}
	raw, e := (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(m)
	if e != nil {
		return nil, e
	}
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if e = d.Decode(&value); e != nil {
		return nil, e
	}
	return json.Marshal(value)
}
