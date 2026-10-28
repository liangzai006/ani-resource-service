package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	PermissionDenied      Reason = "PERMISSION_DENIED"
	PublicEgressNotReady  Reason = "PUBLIC_EGRESS_NOT_READY"
	EIPPoolExhausted      Reason = "EIP_POOL_EXHAUSTED"
	EIPInUse              Reason = "EIP_IN_USE"
	VPCSnatExists         Reason = "VPC_SNAT_EXISTS"
	ProviderStateMismatch Reason = "PROVIDER_STATE_MISMATCH"
)

// EgressAuthorization is resolved by an inbound identity adapter. Neither
// Attribution nor a client supplied tenant/role is an authorization decision.
// The nil/default implementation denies access; IAM integration is independent.
type EgressAuthorization interface {
	Tenant(context.Context, string) (string, Attribution, error)
	Platform(context.Context) (Attribution, error)
}

type egressCallerKey struct{}
type EgressCaller struct {
	TenantID              string
	Attribution           Attribution
	PlatformAdministrator bool
	DelegatedTenants      []string
}

// WithEgressCaller is for a trusted inbound adapter or an explicitly controlled
// test harness. Do not install it from unverified headers or request fields.
func WithEgressCaller(ctx context.Context, caller EgressCaller) context.Context {
	caller.DelegatedTenants = slices.Clone(caller.DelegatedTenants)
	return context.WithValue(ctx, egressCallerKey{}, caller)
}

// EgressCallerOf returns the trusted caller installed by a trusted inbound
// adapter. It reports false when no caller was installed; business code must go
// through EgressAuthorization, not read the context directly.
func EgressCallerOf(ctx context.Context) (EgressCaller, bool) {
	c, ok := ctx.Value(egressCallerKey{}).(EgressCaller)
	return c, ok
}

type ContextEgressAuthorization struct{}

func (ContextEgressAuthorization) Tenant(ctx context.Context, target string) (string, Attribution, error) {
	c, ok := ctx.Value(egressCallerKey{}).(EgressCaller)
	if !ok {
		return "", Attribution{}, Fail(PermissionDenied, "trusted tenant context is required")
	}
	if target == "" {
		target = c.TenantID
	}
	tenant, err := ParseTenant(target)
	if err != nil {
		return "", Attribution{}, err
	}
	own, _ := ParseTenant(c.TenantID)
	if tenant != own && !slices.Contains(c.DelegatedTenants, tenant) {
		return "", Attribution{}, Fail(PermissionDenied, "tenant delegation is required")
	}
	if err = validateAttribution(c.Attribution); err != nil {
		return "", Attribution{}, err
	}
	return tenant, c.Attribution, nil
}
func (ContextEgressAuthorization) Platform(ctx context.Context) (Attribution, error) {
	c, ok := ctx.Value(egressCallerKey{}).(EgressCaller)
	if !ok || !c.PlatformAdministrator {
		return Attribution{}, Fail(PermissionDenied, "platform authorization is required")
	}
	return c.Attribution, validateAttribution(c.Attribution)
}

type EgressMetadata struct {
	ID, Name, Description string
	State                 ResourceState
	Reason                Reason
	Version               int64
	CreatedAt, UpdatedAt  time.Time
	ObservedAt            *time.Time
	ObservationStale      bool
	LastOperationID       string
}

// EIPBindingTarget is the exclusive address claim. BindingID remains the legacy
// SNAT-only field; a load balancer claim has a target and a non-unbound state
// while its legacy BindingID is empty.
type EIPBindingTarget struct {
	Kind, ID, State string
}
type EIP struct {
	EgressMetadata
	TenantID, Address, BindingID, BindingState string
	BindingTarget                              *EIPBindingTarget `json:",omitempty"`
	Scope                                      string            `json:",omitempty"`
	ManagedBy                                  string            `json:",omitempty"`
}
type VPCSnatBinding struct {
	EgressMetadata
	TenantID, VPCID, EIPID, EIPAddress string
	Purpose                            string `json:",omitempty"`
	DesiredEnabled                     bool
	AppliedEnabled                     *bool
}
type PublicPoolConfig struct {
	// Empty scope preserves the original public intent fingerprint and replay.
	Scope                               string   `json:",omitempty"`
	DefaultVPCName                      string   `json:",omitempty"`
	DefaultVPCUID                       string   `json:",omitempty"`
	IntranetNetworks                    []string `json:",omitempty"`
	Mode, GatewayID, CIDR, OVNGatewayIP string
	ExcludedIPs                         []string
	VlanNetworkID, UpstreamGatewayIP    string
}
type PublicPoolVerification struct {
	ProviderSourceRevision                        string
	ProviderImageDigests                          []string
	TopologyFingerprint, EvidenceReference, Scope string
	VerifiedAt, ExpiresAt                         time.Time
}
type DeviceConfig struct {
	DeviceName, InventoryFingerprint string
	Nodes                            []NodeInterface
}
type VlanConfig struct {
	DeviceID        string
	VlanID          int32
	VirtualSwitchID string              `json:",omitempty"`
	ClusterID       string              `json:",omitempty"`
	DeviceName      string              `json:",omitempty"`
	GroupName       string              `json:",omitempty"`
	Nodes           []VirtualSwitchNode `json:",omitempty"`
}
type PlatformResource struct {
	EgressMetadata
	ClusterID, Kind              string
	Device                       *DeviceConfig
	Vlan                         *VlanConfig
	Pool                         *PublicPoolConfig
	AllocationEnabled, IsDefault bool
	ConfigRevision               int64
	TopologyFingerprint          string
	ObservedProviderImages       []string
	Verification                 *PublicPoolVerification
}

// NodeInterface contains actual link/address/route/OVS facts collected inside
// the Kubernetes node network namespace. Node.status.addresses is insufficient.
type NodeInterface struct {
	NodeName, NodeUID, Name, Kind, MAC              string
	MTU                                             int32
	LinkUp, Carrier                                 bool
	Addresses                                       []string
	Master                                          string
	OVSManaged, KCManaged, DefaultRoute, Management bool
	// KCBridgeReady is collected from the real OVS bridge, vendor and mapping.
	// Config identity/ownership are supplied by the provider, never the caller.
	KCBridgeReady, KCConfigured        bool
	KCConfigUID, KCDeviceOwner         string
	KCBridgeNetworks                   []string `json:",omitempty"`
	Selectable                         bool
	UnavailableReasons, VlanNetworkIDs []string
	ObservedAt                         time.Time
}
type InterfaceInventory struct {
	CollectedAt time.Time
	Items       []NodeInterface
	Fingerprint string
}

type EgressIntent struct {
	TenantID, Kind, ID, Name, Description, IdempotencyKey, VPCID, EIPID string
	Enabled                                                             bool
	ExpectedVersion                                                     int64
}

func (i EgressIntent) Fingerprint() string {
	i.IdempotencyKey = ""
	return egressFingerprint(i)
}

type PlatformIntent struct {
	Kind, ID, Name, Description, IdempotencyKey string
	ExpectedVersion                             int64
	Enabled                                     bool
	Device                                      *DeviceConfig
	Vlan                                        *VlanConfig
	Pool                                        *PublicPoolConfig
	Verification                                *PublicPoolVerification
}

func (i PlatformIntent) Fingerprint() string {
	i.IdempotencyKey = ""
	// Observation timestamps never change replay identity. The accepted node
	// set/facts are pinned by the caller's inventory fingerprint.
	if i.Device != nil {
		d := *i.Device
		d.Nodes = nil
		i.Device = &d
	}
	return egressFingerprint(i)
}

// FingerprintVersion retains the exact v1 struct encoding above. New fields
// are omitempty and v2 never includes server-generated registration/binding facts.
func (i PlatformIntent) FingerprintVersion() int32 {
	if i.Vlan != nil && i.Vlan.VirtualSwitchID != "" {
		return 2
	}
	return 1
}

func (i PlatformIntent) VersionedFingerprint() string {
	if i.FingerprintVersion() == 1 {
		return i.Fingerprint()
	}
	return egressFingerprint(struct {
		Version                                                         int
		Kind, Name, Description, ClusterID, VirtualSwitchID, DeviceName string
		VlanID                                                          int32
	}{2, i.Kind, i.Name, i.Description, i.Vlan.ClusterID, i.Vlan.VirtualSwitchID, i.Vlan.DeviceName, i.Vlan.VlanID})
}
func egressFingerprint(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type EgressRepository interface {
	AcceptEIP(context.Context, EgressIntent, Attribution, time.Duration) (EIP, error)
	GetEIP(context.Context, string, string) (EIP, error)
	ListEIPs(context.Context, string, VPCFilter) ([]EIP, int64, error)
	DeleteEIP(context.Context, string, string, Attribution) (EIP, error)
	AcceptSnat(context.Context, EgressIntent, Attribution, time.Duration) (VPCSnatBinding, error)
	GetSnat(context.Context, string, string, bool) (VPCSnatBinding, error)
	DeleteSnat(context.Context, string, string, Attribution) (VPCSnatBinding, error)
	AcceptPlatform(context.Context, PlatformIntent, Attribution, time.Duration) (PlatformResource, error)
	GetPlatform(context.Context, string, string) (PlatformResource, error)
	ListPlatform(context.Context, string, VPCFilter) ([]PlatformResource, int64, error)
	DeletePlatform(context.Context, string, string, Attribution) (PlatformResource, error)
	GetPlatformOperation(context.Context, string) (Operation, error)
}
type EgressInfrastructure interface {
	ListNodeInterfaces(context.Context) (InterfaceInventory, error)
	ValidatePublicPool(context.Context, PublicPoolConfig) error
}

// Intranet infrastructure is separate so public-only providers cannot assert
// base connectivity without implementing its actual gateway and route contract.
type IntranetPoolInfrastructure interface {
	ValidateIntranetPool(context.Context, PublicPoolConfig) error
}
type CapabilityObservation struct {
	Ready            bool
	Reason           Reason
	ObservedAt       *time.Time
	ObservationStale bool
}
type PlatformNetworkCapabilities struct {
	BaseConnectivity, PublicAddress, LoadBalancer CapabilityObservation
}
type PlatformCapabilitiesRepository interface {
	GetPlatformCapabilities(context.Context, time.Duration) (PlatformNetworkCapabilities, error)
}
type Egress struct {
	repository     EgressRepository
	infrastructure EgressInfrastructure
	authorization  EgressAuthorization
	cursor         *Network
	freshness      time.Duration
	now            func() time.Time
}

func NewEgress(r EgressRepository, p EgressInfrastructure, a EgressAuthorization, key []byte, freshness time.Duration, now func() time.Time) (*Egress, error) {
	if r == nil || p == nil || a == nil || len(key) < 32 || freshness <= 0 || now == nil {
		return nil, fmt.Errorf("egress repositories, authorization, cursor key and freshness are required")
	}
	return &Egress{repository: r, infrastructure: p, authorization: a, cursor: &Network{cursorKey: slices.Clone(key)}, freshness: freshness, now: now}, nil
}
func (e *Egress) metadata(v EgressMetadata) EgressMetadata {
	v.ObservationStale = v.ObservedAt == nil || v.ObservedAt.After(e.now()) || e.now().Sub(*v.ObservedAt) > e.freshness
	return v
}
func (e *Egress) eip(v EIP) EIP { v.EgressMetadata = e.metadata(v.EgressMetadata); return v }
func (e *Egress) snat(v VPCSnatBinding) VPCSnatBinding {
	v.EgressMetadata = e.metadata(v.EgressMetadata)
	if v.ObservationStale {
		v.AppliedEnabled = nil
	}
	return v
}
func (e *Egress) platform(v PlatformResource) PlatformResource {
	v.EgressMetadata = e.metadata(v.EgressMetadata)
	return v
}
func normalizeEgressName(name, description, key string) (string, error) {
	// Reuse the established text/key grammar without exposing a CIDR input.
	v, err := NewVPCIntent("00000000-0000-0000-0000-000000000001", name, "10.0.0.0/24", description, key)
	return v.Name, err
}
func validEgressID(id, prefix string) bool {
	return regexp.MustCompile("^" + prefix + "_[0-9a-f]{32}$").MatchString(id)
}
func (e *Egress) CreateEIP(ctx context.Context, i EgressIntent) (EIP, error) {
	tenant, a, err := e.authorization.Tenant(ctx, i.TenantID)
	if err != nil {
		return EIP{}, err
	}
	i = EgressIntent{TenantID: tenant, Kind: "create_eip", Name: i.Name, Description: i.Description, IdempotencyKey: i.IdempotencyKey}
	i.Name, err = normalizeEgressName(i.Name, i.Description, i.IdempotencyKey)
	if err != nil {
		return EIP{}, err
	}
	// Dynamic readiness and fixed pool selection happen after durable replay lookup.
	return e.repository.AcceptEIP(ctx, i, a, e.freshness)
}
func (e *Egress) GetEIP(ctx context.Context, tenant, id string) (EIP, error) {
	tenant, _, err := e.authorization.Tenant(ctx, tenant)
	if err != nil {
		return EIP{}, err
	}
	if !validEgressID(id, "eip") {
		return EIP{}, Fail(ResourceNotFound, "EIP not found")
	}
	v, err := e.repository.GetEIP(ctx, tenant, id)
	return e.eip(v), err
}
func (e *Egress) DeleteEIP(ctx context.Context, tenant, id string) (EIP, error) {
	tenant, a, err := e.authorization.Tenant(ctx, tenant)
	if err != nil {
		return EIP{}, err
	}
	if !validEgressID(id, "eip") {
		return EIP{}, Fail(ResourceNotFound, "EIP not found")
	}
	v, err := e.repository.DeleteEIP(ctx, tenant, id, a)
	return e.eip(v), err
}
func (e *Egress) BindVPCSnat(ctx context.Context, i EgressIntent) (VPCSnatBinding, error) {
	tenant, a, err := e.authorization.Tenant(ctx, i.TenantID)
	if err != nil {
		return VPCSnatBinding{}, err
	}
	if !validVPCID(i.VPCID) || !validEgressID(i.EIPID, "eip") {
		return VPCSnatBinding{}, Fail(ResourceNotFound, "network resource not found")
	}
	if _, err = normalizeEgressName("snat", "", i.IdempotencyKey); err != nil {
		return VPCSnatBinding{}, err
	}
	i = EgressIntent{TenantID: tenant, Kind: "bind_snat", VPCID: i.VPCID, EIPID: i.EIPID, Enabled: true, IdempotencyKey: i.IdempotencyKey}
	return e.repository.AcceptSnat(ctx, i, a, e.freshness)
}
func (e *Egress) SetVPCSnatEnabled(ctx context.Context, i EgressIntent) (VPCSnatBinding, error) {
	tenant, a, err := e.authorization.Tenant(ctx, i.TenantID)
	if err != nil {
		return VPCSnatBinding{}, err
	}
	if !validEgressID(i.ID, "snat") {
		return VPCSnatBinding{}, Fail(ResourceNotFound, "SNAT binding not found")
	}
	if _, err = normalizeEgressName("snat", "", i.IdempotencyKey); err != nil {
		return VPCSnatBinding{}, err
	}
	if i.ExpectedVersion < 1 {
		return VPCSnatBinding{}, Fail(InvalidArgument, "expected_version is required")
	}
	i = EgressIntent{TenantID: tenant, Kind: "set_snat_enabled", ID: i.ID, Enabled: i.Enabled, ExpectedVersion: i.ExpectedVersion, IdempotencyKey: i.IdempotencyKey}
	return e.repository.AcceptSnat(ctx, i, a, e.freshness)
}
func (e *Egress) GetVPCSnat(ctx context.Context, tenant, id string, byVPC bool) (VPCSnatBinding, error) {
	tenant, _, err := e.authorization.Tenant(ctx, tenant)
	if err != nil {
		return VPCSnatBinding{}, err
	}
	if (byVPC && !validVPCID(id)) || (!byVPC && !validEgressID(id, "snat")) {
		return VPCSnatBinding{}, Fail(ResourceNotFound, "SNAT binding not found")
	}
	v, err := e.repository.GetSnat(ctx, tenant, id, byVPC)
	return e.snat(v), err
}
func (e *Egress) DeleteVPCSnatBinding(ctx context.Context, tenant, id string) (VPCSnatBinding, error) {
	tenant, a, err := e.authorization.Tenant(ctx, tenant)
	if err != nil {
		return VPCSnatBinding{}, err
	}
	if !validEgressID(id, "snat") {
		return VPCSnatBinding{}, Fail(ResourceNotFound, "SNAT binding not found")
	}
	v, err := e.repository.DeleteSnat(ctx, tenant, id, a)
	return e.snat(v), err
}
func (e *Egress) listFilter(tenant, kind string, r ListVPCs) (VPCFilter, error) {
	f, err := normalizeList(r.Name, r.State, r.Limit)
	if err != nil {
		return f, err
	}
	if r.Cursor != "" {
		c, err := e.cursor.decodeCursor(r.Cursor)
		if err != nil || c.Version != 1 || c.Kind != kind || c.TenantID != tenant || c.Name != f.Name || c.State != f.State || c.ID == "" || c.CreatedAt.IsZero() {
			return f, Fail(InvalidCursor, "cursor does not match query")
		}
		f.AfterID, f.AfterCreatedAt = c.ID, c.CreatedAt
	}
	return f, nil
}
func (e *Egress) nextCursor(tenant, kind string, f VPCFilter, v EgressMetadata) string {
	return e.cursor.encodeCursor(vpcCursor{Version: 1, Kind: kind, TenantID: tenant, Name: f.Name, State: f.State, ID: v.ID, CreatedAt: v.CreatedAt})
}
func (e *Egress) ListEIPs(ctx context.Context, r ListVPCs) ([]EIP, string, int64, error) {
	tenant, _, err := e.authorization.Tenant(ctx, r.TenantID)
	if err != nil {
		return nil, "", 0, err
	}
	f, err := e.listFilter(tenant, "eip", r)
	if err != nil {
		return nil, "", 0, err
	}
	rows, total, err := e.repository.ListEIPs(ctx, tenant, f)
	if err != nil {
		return nil, "", 0, err
	}
	next := ""
	if len(rows) == int(f.Limit) {
		rows = rows[:len(rows)-1]
		next = e.nextCursor(tenant, "eip", f, rows[len(rows)-1].EgressMetadata)
	}
	for j := range rows {
		rows[j] = e.eip(rows[j])
	}
	return rows, next, total, nil
}

func (e *Egress) ListNodeInterfaces(ctx context.Context, node string) (InterfaceInventory, error) {
	if _, err := e.authorization.Platform(ctx); err != nil {
		return InterfaceInventory{}, err
	}
	inventory, err := e.infrastructure.ListNodeInterfaces(ctx)
	if err != nil {
		return inventory, err
	}
	if node != "" {
		inventory.Items = slices.DeleteFunc(inventory.Items, func(v NodeInterface) bool { return v.NodeName != node })
	}
	return inventory, nil
}
func (e *Egress) CreatePlatform(ctx context.Context, i PlatformIntent) (PlatformResource, error) {
	a, err := e.authorization.Platform(ctx)
	if err != nil {
		return PlatformResource{}, err
	}
	i.Name, err = normalizeEgressName(i.Name, i.Description, i.IdempotencyKey)
	if err != nil {
		return PlatformResource{}, err
	}
	i.ID = ""
	i.ExpectedVersion = 0
	i.Enabled = false
	i.Verification = nil
	switch i.Kind {
	case "adopt_device":
		if i.Device == nil || i.Vlan != nil || i.Pool != nil || !regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`).MatchString(i.Device.DeviceName) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(i.Device.InventoryFingerprint) {
			return PlatformResource{}, Fail(InvalidArgument, "device and inventory fingerprint are required")
		}
		d := *i.Device
		d.Nodes = nil
		i.Device = &d
		// Inventory is collected/revalidated by the repository admission collaborator
		// after replay lookup, and again at the provider mutation boundary.
	case "create_vlan":
		if i.Vlan == nil || i.Device != nil || i.Pool != nil || i.Vlan.VlanID < 0 || i.Vlan.VlanID > 4094 {
			return PlatformResource{}, Fail(InvalidArgument, "device_id and vlan_id 0..4094 are required")
		}
		v := i.Vlan
		if v.VirtualSwitchID != "" || v.ClusterID != "" || v.DeviceName != "" {
			if v.DeviceID != "" || !validEgressID(v.VirtualSwitchID, "vsw") || v.ClusterID == "" || !regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`).MatchString(v.DeviceName) {
				return PlatformResource{}, Fail(InvalidArgument, "v2 requires cluster_id, virtual_switch_id and device_name, mutually exclusive with device_id")
			}
		} else if !validEgressID(v.DeviceID, "device") {
			return PlatformResource{}, Fail(InvalidArgument, "device_id is required for legacy replay")
		}
		i.Vlan = &VlanConfig{DeviceID: v.DeviceID, VlanID: v.VlanID, VirtualSwitchID: v.VirtualSwitchID, ClusterID: v.ClusterID, DeviceName: v.DeviceName}
	case "create_egress_gateway":
		if i.Device != nil || i.Vlan != nil || i.Pool != nil {
			return PlatformResource{}, Fail(InvalidArgument, "gateway has no mode or device configuration")
		}
	case "create_public_pool", "create_intranet_pool":
		if i.Device != nil || i.Vlan != nil || i.Pool == nil {
			return PlatformResource{}, Fail(InvalidArgument, "pool configuration is required")
		}
		var normalized PublicPoolConfig
		var err error
		if i.Kind == "create_intranet_pool" {
			normalized, err = NormalizeIntranetPool(*i.Pool)
		} else {
			normalized, err = NormalizePublicPool(*i.Pool)
		}
		if err != nil {
			return PlatformResource{}, err
		}
		i.Pool = &normalized
	default:
		return PlatformResource{}, Fail(InvalidArgument, "invalid platform creation")
	}
	return e.repository.AcceptPlatform(ctx, i, a, e.freshness)
}
func platformPrefix(kind string) string {
	switch kind {
	case "device":
		return "device"
	case "vlan":
		return "vlan"
	case "egress_gateway":
		return "egw"
	case "public_pool", "intranet_pool":
		return "pool"
	}
	return "invalid"
}
func (e *Egress) GetPlatform(ctx context.Context, kind, id string) (PlatformResource, error) {
	if _, err := e.authorization.Platform(ctx); err != nil {
		return PlatformResource{}, err
	}
	if !validEgressID(id, platformPrefix(kind)) {
		return PlatformResource{}, Fail(ResourceNotFound, "platform resource not found")
	}
	v, err := e.repository.GetPlatform(ctx, kind, id)
	return e.platform(v), err
}
func (e *Egress) ListPlatform(ctx context.Context, kind string, r ListVPCs) ([]PlatformResource, string, int64, error) {
	if _, err := e.authorization.Platform(ctx); err != nil {
		return nil, "", 0, err
	}
	if platformPrefix(kind) == "invalid" {
		return nil, "", 0, Fail(InvalidArgument, "invalid platform kind")
	}
	f, err := e.listFilter("", kind, r)
	if err != nil {
		return nil, "", 0, err
	}
	rows, total, err := e.repository.ListPlatform(ctx, kind, f)
	if err != nil {
		return nil, "", 0, err
	}
	next := ""
	if len(rows) == int(f.Limit) {
		rows = rows[:len(rows)-1]
		next = e.nextCursor("", kind, f, rows[len(rows)-1].EgressMetadata)
	}
	for j := range rows {
		rows[j] = e.platform(rows[j])
	}
	return rows, next, total, nil
}
func (e *Egress) DeletePlatform(ctx context.Context, kind, id string) (PlatformResource, error) {
	a, err := e.authorization.Platform(ctx)
	if err != nil {
		return PlatformResource{}, err
	}
	if kind == "device" || !validEgressID(id, platformPrefix(kind)) {
		return PlatformResource{}, Fail(ResourceNotFound, "deletable platform resource not found")
	}
	v, err := e.repository.DeletePlatform(ctx, kind, id, a)
	return e.platform(v), err
}
func (e *Egress) SetPool(ctx context.Context, i PlatformIntent) (PlatformResource, error) {
	a, err := e.authorization.Platform(ctx)
	if err != nil {
		return PlatformResource{}, err
	}
	if !validEgressID(i.ID, "pool") {
		return PlatformResource{}, Fail(ResourceNotFound, "pool not found")
	}
	if i.ExpectedVersion < 1 {
		return PlatformResource{}, Fail(InvalidArgument, "expected_version is required")
	}
	if _, err = normalizeEgressName("pool", "", i.IdempotencyKey); err != nil {
		return PlatformResource{}, err
	}
	i.Name = ""
	i.Description = ""
	i.Device = nil
	i.Vlan = nil
	i.Pool = nil
	switch i.Kind {
	case "set_pool_allocation", "set_default_pool", "set_intranet_pool_allocation", "set_default_intranet_pool":
		i.Verification = nil
	case "verify_public_pool", "verify_intranet_pool":
		if i.Verification == nil {
			return PlatformResource{}, Fail(InvalidArgument, "verification is required")
		}
		scope := "public"
		if i.Kind == "verify_intranet_pool" {
			scope = "intranet"
		}
		if err = ValidatePoolVerificationForScope(*i.Verification, scope, e.now()); err != nil {
			return PlatformResource{}, err
		}
	default:
		return PlatformResource{}, Fail(InvalidArgument, "invalid pool action")
	}
	return e.repository.AcceptPlatform(ctx, i, a, e.freshness)
}
func (e *Egress) GetPlatformOperation(ctx context.Context, id string) (Operation, error) {
	if _, err := e.authorization.Platform(ctx); err != nil {
		return Operation{}, err
	}
	if _, err := ParseTenant(id); err != nil {
		return Operation{}, Fail(ResourceNotFound, "operation not found")
	}
	return e.repository.GetPlatformOperation(ctx, id)
}

func NormalizePublicPool(p PublicPoolConfig) (PublicPoolConfig, error) {
	bad := func() (PublicPoolConfig, error) {
		return p, Fail(InvalidArgument, "invalid public pool topology or reserved address")
	}
	cidr, err := netip.ParsePrefix(p.CIDR)
	if (p.Scope != "" && p.Scope != "public") || p.DefaultVPCName != "" || p.DefaultVPCUID != "" || len(p.IntranetNetworks) != 0 {
		return bad()
	}
	if err != nil || !cidr.Addr().Is4() || cidr != cidr.Masked() || cidr.Bits() > 30 || (p.GatewayID != "" && !validEgressID(p.GatewayID, "egw")) {
		return bad()
	}
	usable := func(raw string) bool {
		ip, err := netip.ParseAddr(raw)
		return err == nil && ip.Is4() && ip.String() == raw && cidr.Contains(ip) && ip != cidr.Addr() && ip != lastPoolAddress(cidr)
	}
	if !usable(p.OVNGatewayIP) {
		return bad()
	}
	switch p.Mode {
	case "overlay":
		if p.VlanNetworkID != "" || p.UpstreamGatewayIP != "" {
			return bad()
		}
	case "underlay":
		if !validEgressID(p.VlanNetworkID, "vlan") || !usable(p.UpstreamGatewayIP) || p.UpstreamGatewayIP == p.OVNGatewayIP {
			return bad()
		}
	default:
		return bad()
	}
	if len(p.ExcludedIPs) > 1024 {
		return bad()
	}
	p.ExcludedIPs = slices.Clone(p.ExcludedIPs)
	p.ExcludedIPs = append(p.ExcludedIPs, p.OVNGatewayIP)
	if p.Mode == "underlay" {
		p.ExcludedIPs = append(p.ExcludedIPs, p.UpstreamGatewayIP)
	}
	for _, r := range p.ExcludedIPs {
		first, last, rangeValue := strings.Cut(r, "..")
		if !rangeValue {
			last = first
		}
		a, e1 := netip.ParseAddr(first)
		b, e2 := netip.ParseAddr(last)
		if e1 != nil || e2 != nil || !a.Is4() || !b.Is4() || a.String() != first || b.String() != last || !cidr.Contains(a) || !cidr.Contains(b) || a.Compare(b) > 0 {
			return bad()
		}
	}
	slices.Sort(p.ExcludedIPs)
	p.ExcludedIPs = slices.Compact(p.ExcludedIPs)
	return p, nil
}
func lastPoolAddress(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	bits := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	bits |= ^uint32(0) >> p.Bits()
	return netip.AddrFrom4([4]byte{byte(bits >> 24), byte(bits >> 16), byte(bits >> 8), byte(bits)})
}
func ValidatePoolVerification(v PublicPoolVerification, now time.Time) error {
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(v.ProviderSourceRevision) || len(v.ProviderImageDigests) == 0 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(v.TopologyFingerprint) || strings.TrimSpace(v.Scope) == "" || strings.TrimSpace(v.EvidenceReference) == "" || len(v.Scope) > 1024 || len(v.EvidenceReference) > 2048 || v.VerifiedAt.IsZero() || v.VerifiedAt.After(now) || !v.ExpiresAt.After(now) || !v.ExpiresAt.After(v.VerifiedAt) {
		return Fail(InvalidArgument, "verification requires exact provider identity, topology, scope and a valid time window")
	}
	for _, d := range v.ProviderImageDigests {
		if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(d) {
			return Fail(InvalidArgument, "provider image digest must be sha256")
		}
	}
	return nil
}

// FilterNodeInterfaces never renews observed_at. An empty/partial/future/stale
// inventory cannot authorize device adoption, even if other links are healthy.
func FilterNodeInterfaces(items []NodeInterface, now time.Time, freshness time.Duration) []NodeInterface {
	result := slices.Clone(items)
	for j := range result {
		v := &result[j]
		v.UnavailableReasons = slices.Clone(v.UnavailableReasons)
		reject := func(r string) { v.UnavailableReasons = append(v.UnavailableReasons, r) }
		if v.NodeName == "" || v.NodeUID == "" || v.Name == "" {
			reject("incomplete_identity")
		}
		if v.ObservedAt.IsZero() || v.ObservedAt.After(now) || now.Sub(v.ObservedAt) > freshness {
			reject("stale_facts")
		}
		if v.Management || v.DefaultRoute {
			reject("management_or_default_route")
		}
		if v.Kind != "device" {
			reject("not_physical_device")
		}
		managedReady := v.KCManaged && v.KCConfigured && v.KCConfigUID != "" && v.KCBridgeReady && v.OVSManaged && v.Master == "ovs-system" && v.MAC != "" && v.LinkUp && v.Carrier
		if (v.Master != "" || v.OVSManaged) && !managedReady {
			reject("already_in_use")
		}
		if (v.KCManaged || v.KCConfigured) && !managedReady {
			reject("managed_device_not_ready")
		}
		if v.KCDeviceOwner != "" {
			reject("already_registered")
		}
		for _, raw := range v.Addresses {
			ip, err := netip.ParsePrefix(raw)
			if err != nil || !ip.Addr().IsLinkLocalUnicast() {
				reject("address_assigned")
				break
			}
		}
		if len(v.VlanNetworkIDs) > 0 {
			reject("vlan_in_use")
		}
		slices.Sort(v.UnavailableReasons)
		v.UnavailableReasons = slices.Compact(v.UnavailableReasons)
		v.Selectable = len(v.UnavailableReasons) == 0
	}
	return result
}
