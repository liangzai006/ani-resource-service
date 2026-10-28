package biz

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// VirtualSwitch is platform metadata. Its empty binding is a valid persisted
// state; a KCN vCluster has no provider object or UID to invent here.
type VirtualSwitch struct {
	EgressMetadata
	ClusterID, GroupName, BindingID, BindingState, MembershipFingerprint string
	BindingVersion                                                       int64
	Nodes                                                                []VirtualSwitchNode
}

type VirtualSwitchNode struct {
	NodeName, NodeUID, MAC, State, Reason string
	// LastObservedAt is the oldest provider evidence supporting the last
	// successful inspection. Failed reads never renew this durable timestamp.
	LastObservedAt time.Time `json:"LastObservedAt,omitzero"`
	Priority       int32
	Allocated      bool
	// Patch intent and acknowledgement survive ambiguous Kubernetes responses.
	ResourceVersion, OriginalValue, OriginalOwner string
	OriginalPresent, Prepared, Applied, Unknown   bool
	DeleteFenced                                  bool
}

type VirtualSwitchIntent struct{ Name, IdempotencyKey string }
type VirtualSwitchFilter struct {
	Name, State    string
	Page, PageSize int32
}

type VirtualSwitchInventory struct {
	Inventory   InterfaceInventory
	Members     []VirtualSwitchNode
	Fingerprint string
}
type VirtualSwitchInfrastructure interface {
	VirtualSwitchInventory(context.Context, string, VirtualSwitch, string) (VirtualSwitchInventory, error)
}
type VirtualSwitchInventoryReader interface {
	ListVirtualSwitchInterfaces(context.Context, string, VirtualSwitch) (InterfaceInventory, error)
}

func (e *Egress) ListVirtualSwitchInterfaces(ctx context.Context, node, cluster, id string) (InterfaceInventory, error) {
	if _, err := e.authorization.Platform(ctx); err != nil {
		return InterfaceInventory{}, err
	}
	if cluster == "" || !validEgressID(id, "vsw") {
		return InterfaceInventory{}, Fail(InvalidArgument, "cluster_id and virtual_switch_id are required together")
	}
	r, err := e.switchRepository()
	if err != nil {
		return InterfaceInventory{}, err
	}
	sw, err := r.GetVirtualSwitch(ctx, id)
	if err != nil {
		return InterfaceInventory{}, err
	}
	if sw.State == Deleting || sw.State == Deleted || (sw.ClusterID != "" && sw.ClusterID != cluster) {
		return InterfaceInventory{}, Fail(ResourceBusy, "switch is unavailable for this cluster")
	}
	infra, ok := e.infrastructure.(VirtualSwitchInventoryReader)
	if !ok {
		return InterfaceInventory{}, Fail(DependencyUnavailable, "scoped interface inventory is unavailable")
	}
	items, err := infra.ListVirtualSwitchInterfaces(ctx, cluster, sw)
	if err != nil {
		return items, err
	}
	if node != "" {
		// A diagnostic subset never carries a full admission fingerprint.
		filtered := items.Items[:0]
		for _, n := range items.Items {
			if n.NodeName == node {
				filtered = append(filtered, n)
			}
		}
		items.Items = filtered
		items.Fingerprint = ""
	}
	return items, nil
}

type VirtualSwitchRepository interface {
	CreateVirtualSwitch(context.Context, VirtualSwitchIntent, Attribution) (VirtualSwitch, error)
	GetVirtualSwitch(context.Context, string) (VirtualSwitch, error)
	ListVirtualSwitches(context.Context, VirtualSwitchFilter) ([]VirtualSwitch, int64, error)
	DeleteVirtualSwitch(context.Context, string, Attribution) (VirtualSwitch, error)
}

var virtualSwitchName = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9_-]{0,61}[a-zA-Z0-9])?$`)

func (e *Egress) switchRepository() (VirtualSwitchRepository, error) {
	r, ok := e.repository.(VirtualSwitchRepository)
	if !ok {
		return nil, Fail(DependencyUnavailable, "virtual switch persistence is unavailable")
	}
	return r, nil
}

func (e *Egress) CreateVirtualSwitch(ctx context.Context, i VirtualSwitchIntent) (VirtualSwitch, error) {
	a, err := e.authorization.Platform(ctx)
	if err != nil {
		return VirtualSwitch{}, err
	}
	if !virtualSwitchName.MatchString(i.Name) || strings.Contains(i.Name, ".") {
		return VirtualSwitch{}, Fail(InvalidArgument, "name must be a label name without dots (1..63 characters)")
	}
	if _, err = normalizeEgressName(i.Name, "", i.IdempotencyKey); err != nil {
		return VirtualSwitch{}, err
	}
	r, err := e.switchRepository()
	if err != nil {
		return VirtualSwitch{}, err
	}
	return r.CreateVirtualSwitch(ctx, i, a)
}

func (e *Egress) GetVirtualSwitch(ctx context.Context, id string) (VirtualSwitch, error) {
	if _, err := e.authorization.Platform(ctx); err != nil {
		return VirtualSwitch{}, err
	}
	if !validEgressID(id, "vsw") {
		return VirtualSwitch{}, Fail(ResourceNotFound, "virtual switch not found")
	}
	r, err := e.switchRepository()
	if err != nil {
		return VirtualSwitch{}, err
	}
	return r.GetVirtualSwitch(ctx, id)
}

func (e *Egress) ListVirtualSwitches(ctx context.Context, f VirtualSwitchFilter) ([]VirtualSwitch, int64, error) {
	if _, err := e.authorization.Platform(ctx); err != nil {
		return nil, 0, err
	}
	if f.Page == 0 {
		f.Page = 1
	}
	if f.PageSize == 0 {
		f.PageSize = 20
	}
	if f.Page < 1 || f.PageSize < 1 || f.PageSize > 100 {
		return nil, 0, Fail(InvalidArgument, "page must be positive and page_size must be 1..100")
	}
	if _, err := normalizeList(f.Name, f.State, 1); err != nil {
		return nil, 0, err
	}
	r, err := e.switchRepository()
	if err != nil {
		return nil, 0, err
	}
	return r.ListVirtualSwitches(ctx, f)
}

func (e *Egress) DeleteVirtualSwitch(ctx context.Context, id string) (VirtualSwitch, error) {
	a, err := e.authorization.Platform(ctx)
	if err != nil {
		return VirtualSwitch{}, err
	}
	if !validEgressID(id, "vsw") {
		return VirtualSwitch{}, Fail(ResourceNotFound, "virtual switch not found")
	}
	r, err := e.switchRepository()
	if err != nil {
		return VirtualSwitch{}, err
	}
	return r.DeleteVirtualSwitch(ctx, id, a)
}

// VirtualSwitchWork owns a separate lease: concurrent VLANs never patch the
// shared group under unrelated per-VLAN leases.
type VirtualSwitchWork struct {
	Switch VirtualSwitch
	Owner  string
	Epoch  int64
	Now    time.Time
}

type VirtualSwitchWorkRepository interface {
	ClaimVirtualSwitch(context.Context, string, time.Duration) (VirtualSwitchWork, bool, error)
	SaveVirtualSwitchNode(context.Context, VirtualSwitchWork, int, VirtualSwitchNode) error
	FinishVirtualSwitch(context.Context, VirtualSwitchWork, string, Reason, time.Duration) error
}

// Read/patch are external boundaries. Implementations must check the complete
// frozen membership and use Node UID/RV plus binding owner CAS for each patch.
type VirtualSwitchGroupProvider interface {
	InspectVirtualSwitchNode(context.Context, VirtualSwitch, VirtualSwitchNode) (VirtualSwitchNode, error)
	PatchVirtualSwitchNode(context.Context, VirtualSwitch, VirtualSwitchNode, bool) (VirtualSwitchNode, error)
}
