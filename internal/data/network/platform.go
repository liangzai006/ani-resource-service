package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func (p *Postgres) UseEgressInfrastructure(infra biz.EgressInfrastructure) {
	p.egressInfrastructure = infra
}
func platformMetadata(r sqlcgen.NetworkPlatformResource) biz.PlatformResource {
	return biz.PlatformResource{EgressMetadata: biz.EgressMetadata{ID: r.ResourceID, Name: r.Name, Description: r.Description, State: biz.ResourceState(r.State), Reason: biz.Reason(r.Reason), Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, ObservedAt: r.ObservedAt, LastOperationID: r.LastOperationID}, Kind: r.Kind, ClusterID: r.ClusterID}
}
func poolConfig(p sqlcgen.NetworkPublicPool) biz.PublicPoolConfig {
	c := biz.PublicPoolConfig{Mode: p.Mode, GatewayID: textValue(p.GatewayID), CIDR: p.Cidr, OVNGatewayIP: p.OvnGatewayIp, ExcludedIPs: p.ExcludedIps, VlanNetworkID: textValue(p.VlanNetworkID), UpstreamGatewayIP: textValue(p.UpstreamGatewayIp), DefaultVPCName: p.DefaultVpcName, DefaultVPCUID: p.DefaultVpcUid, IntranetNetworks: p.IntranetNetworks}
	if p.Scope == "intranet" {
		c.Scope = "intranet"
	}
	return c
}
func platformSnapshot(ctx context.Context, q *sqlcgen.Queries, r sqlcgen.NetworkPlatformResource) (biz.PlatformResource, error) {
	v := platformMetadata(r)
	switch r.Kind {
	case "device":
		d, err := q.GetDeviceAdoption(ctx, sqlcgen.GetDeviceAdoptionParams{ClusterID: r.ClusterID, ResourceID: r.ResourceID})
		if err != nil {
			return v, databaseFailure(err)
		}
		config := biz.DeviceConfig{DeviceName: d.DeviceName, InventoryFingerprint: d.InventoryFingerprint}
		if err = json.Unmarshal(d.NodeProgress, &config.Nodes); err != nil {
			return v, databaseFailure(err)
		}
		if len(config.Nodes) == 0 {
			if err = json.Unmarshal(d.NodeInventory, &config.Nodes); err != nil {
				return v, databaseFailure(err)
			}
		}
		v.Device = &config
	case "vlan":
		d, err := q.GetVlanNetwork(ctx, sqlcgen.GetVlanNetworkParams{ClusterID: r.ClusterID, ResourceID: r.ResourceID})
		if err != nil {
			return v, databaseFailure(err)
		}
		v.Vlan = &biz.VlanConfig{DeviceID: d.DeviceID, VlanID: d.VlanID}
		if d.VirtualSwitchID != nil {
			s, err := q.GetVirtualSwitch(ctx, sqlcgen.GetVirtualSwitchParams{ResourceID: *d.VirtualSwitchID})
			if err != nil {
				return v, databaseFailure(err)
			}
			device, err := q.GetDeviceAdoption(ctx, sqlcgen.GetDeviceAdoptionParams{ClusterID: r.ClusterID, ResourceID: d.DeviceID})
			if err != nil {
				return v, databaseFailure(err)
			}
			v.Vlan.VirtualSwitchID = s.ResourceID
			v.Vlan.ClusterID = r.ClusterID
			v.Vlan.GroupName = s.Name
			v.Vlan.DeviceName = device.DeviceName
			if err = json.Unmarshal(s.Nodes, &v.Vlan.Nodes); err != nil {
				return v, databaseFailure(err)
			}
		}
	case "public_pool":
		d, err := q.GetPublicPool(ctx, sqlcgen.GetPublicPoolParams{ClusterID: r.ClusterID, ResourceID: r.ResourceID})
		if err != nil {
			return v, databaseFailure(err)
		}
		config := poolConfig(d)
		v.Pool = &config
		v.ConfigRevision = d.ConfigRevision
		topology, err := q.PublicPoolTopology(ctx, sqlcgen.PublicPoolTopologyParams{ClusterID: r.ClusterID, ResourceID: r.ResourceID})
		if err != nil {
			return v, databaseFailure(err)
		}
		v.TopologyFingerprint, err = addressPoolTopologyFor(ctx, q, d, topology)
		if err != nil {
			return v, err
		}
		v.ObservedProviderImages = r.ProviderImages
		v.AllocationEnabled = d.AllocationEnabled
		if len(d.Verification) > 0 {
			var verification biz.PublicPoolVerification
			if err = json.Unmarshal(d.Verification, &verification); err != nil {
				return v, databaseFailure(err)
			}
			v.Verification = &verification
		}
		if d.Scope == "intranet" {
			def, err := q.GetDefaultIntranetPool(ctx, sqlcgen.GetDefaultIntranetPoolParams{ClusterID: r.ClusterID})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return v, databaseFailure(err)
			}
			v.IsDefault = def.PoolID == r.ResourceID
			v.Kind = "intranet_pool"
		} else {
			def, err := q.GetDefaultPublicPool(ctx, sqlcgen.GetDefaultPublicPoolParams{ClusterID: r.ClusterID})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return v, databaseFailure(err)
			}
			v.IsDefault = def.PoolID == r.ResourceID
		}
	}
	return v, nil
}
func (p *Postgres) GetPlatform(ctx context.Context, kind, id string) (biz.PlatformResource, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	r, err := q.GetPlatform(ctx, sqlcgen.GetPlatformParams{ClusterID: p.placement.ClusterID, Kind: storagePlatformKind(kind), ResourceID: id})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	v, err := platformSnapshot(ctx, q, r)
	if err == nil && v.Kind != kind {
		return biz.PlatformResource{}, biz.Fail(biz.ResourceNotFound, "platform resource not found")
	}
	return v, err
}
func (p *Postgres) ListPlatform(ctx context.Context, kind string, f biz.VPCFilter) ([]biz.PlatformResource, int64, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	var rows []sqlcgen.NetworkPlatformResource
	var total int64
	if kind == "intranet_pool" {
		total, err = q.CountIntranetPools(ctx, sqlcgen.CountIntranetPoolsParams{ClusterID: p.placement.ClusterID, NameFilter: f.Name, StateFilter: f.State})
		if err != nil {
			return nil, 0, databaseFailure(err)
		}
		rows, err = q.ListIntranetPools(ctx, sqlcgen.ListIntranetPoolsParams{ClusterID: p.placement.ClusterID, NameFilter: f.Name, StateFilter: f.State, AfterID: f.AfterID, AfterCreatedAt: f.AfterCreatedAt, MaxResults: f.Limit})
	} else {
		total, err = q.CountPlatform(ctx, sqlcgen.CountPlatformParams{ClusterID: p.placement.ClusterID, Kind: kind, NameFilter: f.Name, StateFilter: f.State})
		if err != nil {
			return nil, 0, databaseFailure(err)
		}
		rows, err = q.ListPlatform(ctx, sqlcgen.ListPlatformParams{ClusterID: p.placement.ClusterID, Kind: kind, NameFilter: f.Name, StateFilter: f.State, AfterID: f.AfterID, AfterCreatedAt: f.AfterCreatedAt, MaxResults: f.Limit})
	}
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	values := make([]biz.PlatformResource, 0, len(rows))
	for _, r := range rows {
		v, err := platformSnapshot(ctx, q, r)
		if err != nil {
			return nil, 0, err
		}
		values = append(values, v)
	}
	return values, total, nil
}
func (p *Postgres) GetPlatformOperation(ctx context.Context, id string) (biz.Operation, error) {
	r, err := p.queries.GetPlatformOperation(ctx, sqlcgen.GetPlatformOperationParams{ClusterID: p.placement.ClusterID, OperationID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		s, e := p.queries.GetVirtualSwitchOperation(ctx, sqlcgen.GetVirtualSwitchOperationParams{OperationID: id})
		if e != nil {
			return biz.Operation{}, databaseFailure(e)
		}
		return biz.Operation{ID: s.OperationID, ResourceID: s.ResourceID, ResourceType: "virtual_switch", Kind: s.Kind, State: biz.OperationState(s.State), Reason: biz.Reason(s.Reason), CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, CompletedAt: s.CompletedAt}, nil
	}
	return biz.Operation{ID: r.OperationID, ResourceID: r.ResourceID, ResourceType: platformOperationResourceKind(r.Kind), Kind: r.Kind, State: biz.OperationState(r.State), Reason: biz.Reason(r.Reason), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, CompletedAt: r.CompletedAt, NextAttemptAt: r.NextAttemptAt}, databaseFailure(err)
}
func platformReplay(ctx context.Context, q *sqlcgen.Queries, cluster string, i biz.PlatformIntent) (biz.PlatformResource, bool, error) {
	prior, err := q.GetPlatformIdempotency(ctx, sqlcgen.GetPlatformIdempotencyParams{ClusterID: cluster, OperationKind: i.Kind, IdempotencyKey: i.IdempotencyKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.PlatformResource{}, false, nil
	}
	if err != nil {
		return biz.PlatformResource{}, false, databaseFailure(err)
	}
	if prior.FingerprintVersion != i.FingerprintVersion() || prior.Fingerprint != i.VersionedFingerprint() {
		return biz.PlatformResource{}, false, biz.Fail(biz.IdempotencyConflict, "key already accepted another platform intent")
	}
	var v biz.PlatformResource
	if err = json.Unmarshal(prior.Response, &v); err != nil {
		return v, false, databaseFailure(err)
	}
	if v.ID != prior.ResourceID || v.ClusterID != cluster || v.LastOperationID != prior.OperationID {
		return v, false, databaseFailure(fmt.Errorf("invalid platform acceptance snapshot"))
	}
	return v, true, nil
}
func (p *Postgres) AcceptPlatform(ctx context.Context, i biz.PlatformIntent, a biz.Attribution, freshness time.Duration) (biz.PlatformResource, error) {
	// Replay is checked before dynamic infrastructure. Slow provider calls occur
	// before the write transaction, then admission rechecks the key and parents.
	if v, found, err := platformReplay(ctx, p.queries, p.placement.ClusterID, i); found || err != nil {
		return v, err
	}
	if i.Kind == "create_vlan" && i.FingerprintVersion() == 2 {
		return p.acceptSwitchVlan(ctx, i, a, freshness)
	}
	if i.Kind == "create_vlan" {
		if _, v09 := p.egressInfrastructure.(biz.VirtualSwitchInfrastructure); v09 {
			return biz.PlatformResource{}, biz.Fail(biz.InvalidArgument, "new VLAN creation requires a virtual switch; legacy exact replay remains supported")
		}
	}
	if i.Kind == "adopt_device" || i.Kind == "create_public_pool" || i.Kind == "create_intranet_pool" {
		if p.egressInfrastructure == nil {
			return biz.PlatformResource{}, biz.Fail(biz.DependencyUnavailable, "platform facts are unavailable")
		}
		if i.Kind == "adopt_device" {
			inventory, err := p.egressInfrastructure.ListNodeInterfaces(ctx)
			if err != nil {
				return biz.PlatformResource{}, err
			}
			if inventory.Fingerprint != i.Device.InventoryFingerprint {
				return biz.PlatformResource{}, biz.Fail(biz.ResourceBusy, "node inventory changed")
			}
			nodes := map[string]bool{}
			selected := map[string]bool{}
			d := *i.Device
			d.Nodes = nil
			for _, v := range inventory.Items {
				nodes[v.NodeUID] = true
				if v.Name == d.DeviceName {
					if !v.Selectable || selected[v.NodeUID] {
						return biz.PlatformResource{}, biz.Fail(biz.ResourceInUse, "device is unavailable on a node")
					}
					d.Nodes = append(d.Nodes, v)
					selected[v.NodeUID] = true
				}
			}
			if len(nodes) == 0 || len(selected) != len(nodes) {
				return biz.PlatformResource{}, biz.Fail(biz.DependencyUnavailable, "device facts must cover all nodes")
			}
			i.Device = &d
		} else if i.Kind == "create_intranet_pool" {
			infra, ok := p.egressInfrastructure.(biz.IntranetPoolInfrastructure)
			if !ok {
				return biz.PlatformResource{}, biz.Fail(biz.DependencyUnavailable, "intranet infrastructure facts are unavailable")
			}
			if err := infra.ValidateIntranetPool(ctx, *i.Pool); err != nil {
				return biz.PlatformResource{}, err
			}
		} else {
			if err := p.egressInfrastructure.ValidatePublicPool(ctx, *i.Pool); err != nil {
				return biz.PlatformResource{}, err
			}
		}
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = q.LockPlatformKey(ctx, sqlcgen.LockPlatformKeyParams{ClusterID: p.placement.ClusterID, OperationKind: i.Kind, IdempotencyKey: i.IdempotencyKey}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if v, found, err := platformReplay(ctx, q, p.placement.ClusterID, i); found || err != nil {
		return v, err
	}
	if err = q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: p.placement.ClusterID}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if i.ID != "" {
		return p.acceptPoolChange(ctx, tx, q, i, a, now, freshness)
	}
	kind, prefix := "", ""
	switch i.Kind {
	case "adopt_device":
		kind, prefix = "device", "device"
	case "create_vlan":
		kind, prefix = "vlan", "vlan"
	case "create_egress_gateway":
		kind, prefix = "egress_gateway", "egw"
	case "create_public_pool", "create_intranet_pool":
		kind, prefix = "public_pool", "pool"
	default:
		return biz.PlatformResource{}, biz.Fail(biz.InvalidArgument, "invalid platform intent")
	}
	checkParent := func(kind, id string) error {
		r, err := q.LockPlatform(ctx, sqlcgen.LockPlatformParams{ClusterID: p.placement.ClusterID, Kind: kind, ResourceID: id})
		if err != nil {
			return databaseFailure(err)
		}
		if !freshResource(r.State, r.ObservedAt, now, freshness) {
			return biz.Fail(biz.PublicEgressNotReady, "platform parent is not ready")
		}
		return nil
	}
	if i.Vlan != nil {
		if err = checkParent("device", i.Vlan.DeviceID); err != nil {
			return biz.PlatformResource{}, err
		}
	}
	if i.Pool != nil {
		if i.Pool.Scope != "intranet" && i.Pool.GatewayID != "" {
			if _, ownerErr := q.ManagedGatewayOwner(ctx, sqlcgen.ManagedGatewayOwnerParams{ClusterID: p.placement.ClusterID, GatewayID: &i.Pool.GatewayID}); ownerErr == nil {
				return biz.PlatformResource{}, biz.Fail(biz.ResourceInUse, "gateway is owned by another public pool")
			} else if !errors.Is(ownerErr, pgx.ErrNoRows) {
				return biz.PlatformResource{}, databaseFailure(ownerErr)
			}
			if err = checkParent("egress_gateway", i.Pool.GatewayID); err != nil {
				return biz.PlatformResource{}, err
			}
		}
		if i.Pool.Mode == "underlay" {
			if err = checkParent("vlan", i.Pool.VlanNetworkID); err != nil {
				return biz.PlatformResource{}, err
			}
		}
		cidr, err := netip.ParsePrefix(i.Pool.CIDR)
		if err != nil {
			return biz.PlatformResource{}, biz.Fail(biz.InvalidArgument, "invalid pool CIDR")
		}
		vpcCount, err := q.PlatformPoolVPCOverlaps(ctx, sqlcgen.PlatformPoolVPCOverlapsParams{ClusterID: p.placement.ClusterID, Cidr: cidr})
		if err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
		if vpcCount > 0 {
			return biz.PlatformResource{}, biz.Fail(biz.ResourceInUse, "address pool CIDR overlaps an accepted VPC")
		}
		n, err := q.PublicPoolOverlaps(ctx, sqlcgen.PublicPoolOverlapsParams{ClusterID: p.placement.ClusterID, Cidr: cidr})
		if err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
		if n > 0 {
			return biz.PlatformResource{}, biz.Fail(biz.ResourceInUse, "address pool CIDR overlaps an existing pool")
		}
	}
	id := newEgressID(prefix)
	op := uuid.NewString()
	row, err := q.InsertPlatformResource(ctx, sqlcgen.InsertPlatformResourceParams{ResourceID: id, Kind: kind, ClusterID: p.placement.ClusterID, Name: i.Name, Description: i.Description, CreatedAt: now, LastOperationID: op, ProviderName: strings.Replace(id, "_", "-", 1), BindingID: uuid.NewString()})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	switch kind {
	case "device":
		nodes, err := json.Marshal(i.Device.Nodes)
		if err != nil {
			return biz.PlatformResource{}, err
		}
		err = q.InsertDeviceAdoption(ctx, sqlcgen.InsertDeviceAdoptionParams{ResourceID: id, ClusterID: p.placement.ClusterID, DeviceName: i.Device.DeviceName, InventoryFingerprint: i.Device.InventoryFingerprint, NodeInventory: nodes})
		if err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
	case "vlan":
		err = q.InsertVlanNetwork(ctx, sqlcgen.InsertVlanNetworkParams{ResourceID: id, ClusterID: p.placement.ClusterID, DeviceID: i.Vlan.DeviceID, VlanID: i.Vlan.VlanID})
	case "egress_gateway":
		err = q.InsertEgressGateway(ctx, sqlcgen.InsertEgressGatewayParams{ResourceID: id, ClusterID: p.placement.ClusterID})
	case "public_pool":
		c := *i.Pool
		managed := c.Scope != "intranet" && c.GatewayID == ""
		if managed {
			c.GatewayID, err = p.acceptManagedGateway(ctx, q, row, a, now)
			if err != nil {
				return biz.PlatformResource{}, err
			}
		}
		if c.Scope == "intranet" {
			err = q.InsertIntranetPool(ctx, sqlcgen.InsertIntranetPoolParams{ResourceID: id, ClusterID: p.placement.ClusterID, Cidr: c.CIDR, OvnGatewayIp: c.OVNGatewayIP, ExcludedIps: c.ExcludedIPs, DefaultVpcName: c.DefaultVPCName, DefaultVpcUid: c.DefaultVPCUID, IntranetNetworks: c.IntranetNetworks})
		} else {
			err = q.InsertPublicPool(ctx, sqlcgen.InsertPublicPoolParams{ResourceID: id, ClusterID: p.placement.ClusterID, Mode: c.Mode, ManagedGateway: managed, GatewayID: &c.GatewayID, Cidr: c.CIDR, OvnGatewayIp: c.OVNGatewayIP, ExcludedIps: c.ExcludedIPs, VlanNetworkID: c.VlanNetworkID, UpstreamGatewayIp: c.UpstreamGatewayIP})
		}
	}
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if err = q.InsertPlatformOperation(ctx, sqlcgen.InsertPlatformOperationParams{ResourceID: id, OperationID: op, Kind: i.Kind, State: string(biz.Queued), CreatedAt: now, NextAttemptAt: &now}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if err = q.InsertPlatformReconciliation(ctx, sqlcgen.InsertPlatformReconciliationParams{ResourceID: id, NextRunAt: now}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	value, err := platformSnapshot(ctx, q, row)
	if err != nil {
		return value, err
	}
	value.ObservationStale = true
	if err = savePlatformAcceptance(ctx, q, i, a, value, now, string(biz.Queued)); err != nil {
		return biz.PlatformResource{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	return value, nil
}
func savePlatformAcceptance(ctx context.Context, q *sqlcgen.Queries, i biz.PlatformIntent, a biz.Attribution, v biz.PlatformResource, now time.Time, state string) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = q.InsertPlatformIdempotency(ctx, sqlcgen.InsertPlatformIdempotencyParams{ClusterID: v.ClusterID, OperationKind: i.Kind, IdempotencyKey: i.IdempotencyKey, Fingerprint: i.VersionedFingerprint(), FingerprintVersion: i.FingerprintVersion(), ResourceID: v.ID, OperationID: v.LastOperationID, Response: body, CreatedAt: now}); err != nil {
		return databaseFailure(err)
	}
	return databaseFailure(q.InsertPlatformHistory(ctx, sqlcgen.InsertPlatformHistoryParams{HistoryID: uuid.NewString(), ResourceID: v.ID, OperationID: &v.LastOperationID, Event: i.Kind + "_accepted", ResourceState: string(v.State), OperationState: state, ActorRef: a.Actor, CallerRef: a.DirectCaller, CreatedAt: now}))
}
func (p *Postgres) acceptPoolChange(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, i biz.PlatformIntent, a biz.Attribution, now time.Time, freshness time.Duration) (biz.PlatformResource, error) {
	row, err := q.LockPlatform(ctx, sqlcgen.LockPlatformParams{ClusterID: p.placement.ClusterID, Kind: "public_pool", ResourceID: i.ID})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if row.Version != i.ExpectedVersion {
		return biz.PlatformResource{}, biz.Fail(biz.ResourceBusy, "pool version changed")
	}
	if row.State == "deleted" || row.State == "deleting" {
		return biz.PlatformResource{}, biz.Fail(biz.ResourceBusy, "pool is being removed")
	}
	op, err := q.GetPlatformOperation(ctx, sqlcgen.GetPlatformOperationParams{ClusterID: p.placement.ClusterID, OperationID: row.LastOperationID})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if op.CompletedAt == nil {
		return biz.PlatformResource{}, biz.Fail(biz.ResourceBusy, "pool has unfinished work")
	}
	pool, err := q.LockPublicPool(ctx, sqlcgen.LockPublicPoolParams{ClusterID: p.placement.ClusterID, ResourceID: i.ID})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if (strings.Contains(i.Kind, "intranet") && pool.Scope != "intranet") || (!strings.Contains(i.Kind, "intranet") && pool.Scope != "public") {
		return biz.PlatformResource{}, biz.Fail(biz.ResourceNotFound, "pool not found")
	}
	topology, err := q.PublicPoolTopology(ctx, sqlcgen.PublicPoolTopologyParams{ClusterID: row.ClusterID, ResourceID: row.ResourceID})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	topologyHash, err := addressPoolTopologyFor(ctx, q, pool, topology)
	if err != nil {
		return biz.PlatformResource{}, err
	}
	switch i.Kind {
	case "verify_public_pool", "verify_intranet_pool":
		if !freshResource(row.State, row.ObservedAt, now, freshness) || i.Verification == nil || i.Verification.TopologyFingerprint != topologyHash || !sameProviderImages(i.Verification.ProviderImageDigests, row.ProviderImages) {
			return biz.PlatformResource{}, biz.Fail(poolUnavailableReason(pool.Scope), "verification does not match ready pool topology")
		}
		if err = biz.ValidatePoolVerificationForScope(*i.Verification, pool.Scope, now); err != nil {
			return biz.PlatformResource{}, err
		}
		b, _ := json.Marshal(i.Verification)
		if err = affected(q.SavePoolVerification(ctx, sqlcgen.SavePoolVerificationParams{ClusterID: p.placement.ClusterID, ResourceID: i.ID, Verification: b, VerificationExpiresAt: &i.Verification.ExpiresAt})); err != nil {
			return biz.PlatformResource{}, err
		}
	case "set_pool_allocation", "set_intranet_pool_allocation":
		if i.Enabled {
			if err = p.poolReady(ctx, q, pool, now, freshness, false); err != nil {
				return biz.PlatformResource{}, err
			}
		}
		if err = affected(q.SetPoolAllocation(ctx, sqlcgen.SetPoolAllocationParams{ClusterID: p.placement.ClusterID, ResourceID: i.ID, AllocationEnabled: i.Enabled})); err != nil {
			return biz.PlatformResource{}, err
		}
	case "set_default_intranet_pool":
		if err = q.SetDefaultIntranetPool(ctx, sqlcgen.SetDefaultIntranetPoolParams{ClusterID: p.placement.ClusterID, PoolID: i.ID}); err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
	case "set_default_pool":
		// The default can be configured while closed; admission remains gated.
		if err = q.SetDefaultPublicPool(ctx, sqlcgen.SetDefaultPublicPoolParams{ClusterID: p.placement.ClusterID, PoolID: i.ID}); err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
	default:
		return biz.PlatformResource{}, biz.Fail(biz.InvalidArgument, "invalid pool command")
	}
	opID := uuid.NewString()
	row, err = q.SetPlatformIntent(ctx, sqlcgen.SetPlatformIntentParams{ClusterID: p.placement.ClusterID, ResourceID: i.ID, Version: i.ExpectedVersion, State: row.State, OperationID: opID})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if err = q.InsertPlatformOperation(ctx, sqlcgen.InsertPlatformOperationParams{ResourceID: i.ID, OperationID: opID, Kind: i.Kind, State: string(biz.Succeeded), CreatedAt: now, CompletedAt: &now}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if err = affected(q.SchedulePlatform(ctx, sqlcgen.SchedulePlatformParams{ResourceID: i.ID})); err != nil {
		return biz.PlatformResource{}, err
	}
	v, err := platformSnapshot(ctx, q, row)
	if err != nil {
		return v, err
	}
	if err = savePlatformAcceptance(ctx, q, i, a, v, now, string(biz.Succeeded)); err != nil {
		return v, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	return v, nil
}
func sameProviderImages(a, b []string) bool {
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return len(a) > 0 && slices.Equal(slices.Compact(a), slices.Compact(b))
}
func (p *Postgres) poolReady(ctx context.Context, q *sqlcgen.Queries, pool sqlcgen.NetworkPublicPool, now time.Time, freshness time.Duration, allocation bool) error {
	unavailable := func() error {
		return biz.Fail(poolUnavailableReason(pool.Scope), "address pool configuration or evidence is not ready")
	}
	if allocation && !pool.AllocationEnabled {
		return unavailable()
	}
	topology, err := q.PublicPoolTopology(ctx, sqlcgen.PublicPoolTopologyParams{ClusterID: pool.ClusterID, ResourceID: pool.ResourceID})
	if err != nil {
		return databaseFailure(err)
	}
	// Current provider health remains mandatory. Manual forwarding evidence is
	// optional for public pools; intranet keeps its independent verification.
	if len(topology.ProviderImages) == 0 {
		return unavailable()
	}
	if pool.Scope != "public" {
		if pool.VerificationExpiresAt == nil || !pool.VerificationExpiresAt.After(now) || len(pool.Verification) == 0 {
			return unavailable()
		}
		var evidence biz.PublicPoolVerification
		topologyHash, err := addressPoolTopologyFor(ctx, q, pool, topology)
		if err != nil {
			return err
		}
		if json.Unmarshal(pool.Verification, &evidence) != nil || evidence.TopologyFingerprint != topologyHash || !sameProviderImages(evidence.ProviderImageDigests, topology.ProviderImages) || biz.ValidatePoolVerificationForScope(evidence, pool.Scope, now) != nil {
			return unavailable()
		}
	}
	refs := [][2]string{{"public_pool", pool.ResourceID}}
	if pool.Scope != "intranet" {
		refs = append(refs, [2]string{"egress_gateway", textValue(pool.GatewayID)})
	}
	if pool.VlanNetworkID != nil {
		refs = append(refs, [2]string{"vlan", *pool.VlanNetworkID})
		vlan, err := q.GetVlanNetwork(ctx, sqlcgen.GetVlanNetworkParams{ClusterID: pool.ClusterID, ResourceID: *pool.VlanNetworkID})
		if err != nil {
			return databaseFailure(err)
		}
		refs = append(refs, [2]string{"device", vlan.DeviceID})
	}
	for _, ref := range refs {
		if ref[0] == "device" {
			d, err := q.GetDeviceAdoption(ctx, sqlcgen.GetDeviceAdoptionParams{ClusterID: pool.ClusterID, ResourceID: ref[1]})
			if err != nil {
				return databaseFailure(err)
			}
			if d.VirtualSwitchID != nil {
				s, err := q.GetVirtualSwitch(ctx, sqlcgen.GetVirtualSwitchParams{ResourceID: *d.VirtualSwitchID})
				if err != nil {
					return databaseFailure(err)
				}
				if d.Retired || s.State != "available" || s.BindingState != "configured" || s.UpdatedAt.After(now) || now.Sub(s.UpdatedAt) > freshness || textValue(d.SwitchBindingID) != textValue(s.BindingID) {
					return unavailable()
				}
				continue // The fresh VLAN observation above proves actual wiring.
			}
		}
		r, err := q.GetPlatform(ctx, sqlcgen.GetPlatformParams{ClusterID: pool.ClusterID, Kind: ref[0], ResourceID: ref[1]})
		if err != nil {
			return databaseFailure(err)
		}
		if !freshResource(r.State, r.ObservedAt, now, freshness) || r.ProviderUid == "" {
			return unavailable()
		}
	}
	return nil
}
func (p *Postgres) DeletePlatform(ctx context.Context, kind, id string, a biz.Attribution) (biz.PlatformResource, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: p.placement.ClusterID}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	row, err := q.LockPlatform(ctx, sqlcgen.LockPlatformParams{ClusterID: p.placement.ClusterID, Kind: storagePlatformKind(kind), ResourceID: id})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	snapshot, err := platformSnapshot(ctx, q, row)
	if err != nil {
		return biz.PlatformResource{}, err
	}
	if snapshot.Kind != kind {
		return biz.PlatformResource{}, biz.Fail(biz.ResourceNotFound, "platform resource not found")
	}
	if row.State == "deleted" || row.State == "deleting" {
		return platformSnapshot(ctx, q, row)
	}
	op, err := q.GetPlatformOperation(ctx, sqlcgen.GetPlatformOperationParams{ClusterID: p.placement.ClusterID, OperationID: row.LastOperationID})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if op.CompletedAt == nil {
		// Owner deletion also cancels a pool waiting for its managed gateway.
		pool, poolErr := q.GetPublicPool(ctx, sqlcgen.GetPublicPoolParams{ClusterID: row.ClusterID, ResourceID: id})
		if kind != "public_pool" || poolErr != nil || !pool.ManagedGateway || op.Kind != "create_public_pool" {
			return biz.PlatformResource{}, biz.Fail(biz.ResourceBusy, "platform creation has unfinished work")
		}
		if err = affected(q.SupersedePlatformCreate(ctx, sqlcgen.SupersedePlatformCreateParams{ResourceID: id, OperationID: op.OperationID})); err != nil {
			return biz.PlatformResource{}, err
		}
	}
	var count int64
	switch kind {
	case "public_pool", "intranet_pool":
		pool, err := q.LockPublicPool(ctx, sqlcgen.LockPublicPoolParams{ClusterID: p.placement.ClusterID, ResourceID: id})
		if err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
		if pool.AllocationEnabled {
			return biz.PlatformResource{}, biz.Fail(biz.ResourceInUse, "close allocation before retiring a pool")
		}
		count, err = q.CountPoolEIPs(ctx, sqlcgen.CountPoolEIPsParams{ClusterID: p.placement.ClusterID, PoolID: id})
		if err == nil && count == 0 {
			count, err = q.CountDefaultEgressPoolReferences(ctx, sqlcgen.CountDefaultEgressPoolReferencesParams{ClusterID: p.placement.ClusterID, PoolID: id})
		}
	case "egress_gateway":
		count, err = q.CountGatewayPools(ctx, sqlcgen.CountGatewayPoolsParams{ClusterID: p.placement.ClusterID, GatewayID: &id})
	case "vlan":
		count, err = q.CountVlanPools(ctx, sqlcgen.CountVlanPoolsParams{ClusterID: p.placement.ClusterID, VlanNetworkID: &id})
	default:
		return biz.PlatformResource{}, biz.Fail(biz.InvalidArgument, "device retirement is a separate infrastructure action")
	}
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if count > 0 {
		return biz.PlatformResource{}, biz.Fail(biz.ResourceInUse, "platform dependencies must be released")
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	opID := uuid.NewString()
	row, err = q.SetPlatformIntent(ctx, sqlcgen.SetPlatformIntentParams{ClusterID: p.placement.ClusterID, ResourceID: id, Version: row.Version, State: string(biz.Deleting), OperationID: opID})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if err = q.InsertPlatformOperation(ctx, sqlcgen.InsertPlatformOperationParams{ResourceID: id, OperationID: opID, Kind: "delete_" + kind, State: string(biz.Queued), CreatedAt: now, NextAttemptAt: &now}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if err = affected(q.SchedulePlatform(ctx, sqlcgen.SchedulePlatformParams{ResourceID: id})); err != nil {
		return biz.PlatformResource{}, err
	}
	if err = q.InsertPlatformHistory(ctx, sqlcgen.InsertPlatformHistoryParams{HistoryID: uuid.NewString(), ResourceID: id, OperationID: &opID, Event: "delete_accepted", ResourceState: string(biz.Deleting), OperationState: string(biz.Queued), ActorRef: a.Actor, CallerRef: a.DirectCaller, CreatedAt: now}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	value, err := platformSnapshot(ctx, q, row)
	if err != nil {
		return value, err
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	return value, nil
}

func platformOperationResourceKind(kind string) string {
	switch kind {
	case "adopt_device":
		return "device"
	case "create_vlan", "delete_vlan":
		return "vlan"
	case "create_egress_gateway", "delete_egress_gateway":
		return "egress_gateway"
	case "create_public_pool", "delete_public_pool", "verify_public_pool", "set_pool_allocation", "set_default_pool":
		return "public_pool"
	case "create_intranet_pool", "delete_intranet_pool", "verify_intranet_pool", "set_intranet_pool_allocation", "set_default_intranet_pool":
		return "intranet_pool"
	}
	return ""
}

func storagePlatformKind(kind string) string {
	if kind == "intranet_pool" {
		return "public_pool"
	}
	return kind
}
func poolUnavailableReason(scope string) biz.Reason {
	if scope == "intranet" {
		return biz.Reason("BASE_CONNECTIVITY_NOT_READY")
	}
	return biz.PublicEgressNotReady
}

// Public hashing retains the legacy JSON shape, including nil optional fields.
// Intranet evidence additionally pins the default router and destination intent.
func addressPoolTopologyHash(pool sqlcgen.NetworkPublicPool, topology sqlcgen.PublicPoolTopologyRow) string {
	if pool.Scope != "intranet" {
		return contentHash(topology)
	}
	return contentHash(struct {
		Topology                             sqlcgen.PublicPoolTopologyRow
		Scope, DefaultVPCName, DefaultVPCUID string
		IntranetNetworks                     []string
	}{topology, pool.Scope, pool.DefaultVpcName, pool.DefaultVpcUid, pool.IntranetNetworks})
}

func addressPoolTopologyFor(ctx context.Context, q *sqlcgen.Queries, pool sqlcgen.NetworkPublicPool, topology sqlcgen.PublicPoolTopologyRow) (string, error) {
	legacy := addressPoolTopologyHash(pool, topology)
	s, err := q.PublicPoolVirtualSwitchTopology(ctx, sqlcgen.PublicPoolVirtualSwitchTopologyParams{ClusterID: pool.ClusterID, ResourceID: pool.ResourceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return legacy, nil
	}
	if err != nil {
		return "", databaseFailure(err)
	}
	// Dynamic patch/RV progress is deliberately absent from this identity proof.
	// It binds the immutable switch/device membership and actual v0.9 mapping.
	return contentHash(struct {
		LegacyFingerprint, ProviderProfile, SwitchID, BindingID, GroupName, MembershipFingerprint, DeviceFingerprint string
		BindingVersion                                                                                               int64
	}{legacy, "kcn-v0.9-vcluster.device:br-device", s.SwitchID, textValue(s.BindingID), s.GroupName, s.MembershipFingerprint, s.DeviceFingerprint, s.BindingVersion}), nil
}
