package vm

import (
	"context"
	"encoding/json"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

func snapshotWire(s biz.Snapshot) *pb.Snapshot {
	completeness := "INCOMPLETE"
	if s.Complete {
		completeness = "COMPLETE"
	}
	out := &pb.Snapshot{SnapshotId: s.ID, SourceVmId: s.SourceVMID, SourceVmUid: s.SourceVMUID, Ready: s.Ready, Completeness: completeness, Included: append([]string(nil), s.Included...), Excluded: append([]string(nil), s.Excluded...), Indications: append([]string(nil), s.Indications...), ObservedAt: observed(s.ObservedAt), Reason: s.Reason}
	for _, d := range s.Inventory {
		out.Disks = append(out.Disks, &pb.Disk{DiskId: d.ID, Role: d.Role, Name: d.Name, SizeGb: d.SizeGB, RequestedBytes: d.RequestedBytes, ObservedCapacityBytes: d.ObservedCapacityBytes, VolumeName: d.VolumeName, Serial: d.Serial, Attached: d.Attached, PreparationState: d.Preparation, Reason: d.Reason})
	}
	return out
}
func (s *Service) snapshotObservation(ctx context.Context, v biz.VM, index int) (biz.Snapshot, error) {
	native, ok := s.native.(biz.SnapshotNativePort)
	if !ok {
		return biz.Snapshot{}, biz.ErrDependency
	}
	if index < 0 || index >= len(v.Snapshots) {
		return biz.Snapshot{}, biz.ErrNotFound
	}
	item := v.Snapshots[index]
	if item.SourceVMID != v.ID || item.SourceVMUID != v.Native.UID {
		return item, biz.ErrOwnership
	}
	observed, err := native.ObserveSnapshot(ctx, v, item)
	if err != nil {
		return item, err
	}
	observed.Complete = biz.SnapshotComplete(observed)
	return observed, nil
}
func (s *Service) ListSnapshots(ctx context.Context, r *pb.GetVMRequest) (*pb.ListSnapshotsResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	v, err := s.repo.Get(ctx, r.TenantId, r.VmId)
	if err != nil {
		return nil, rpcError(err)
	}
	result := &pb.ListSnapshotsResponse{}
	// Workers persist snapshot observations. Listing that projection must not
	// reopen every native dependency; detail and restore paths recheck it live.
	for _, item := range v.Snapshots {
		if item.Tombstone {
			continue
		}
		if item.SourceVMID != v.ID || item.SourceVMUID != v.Native.UID {
			return nil, rpcError(biz.ErrOwnership)
		}
		result.Items = append(result.Items, snapshotWire(item))
	}
	return result, nil
}
func (s *Service) GetSnapshot(ctx context.Context, r *pb.GetSnapshotRequest) (*pb.Snapshot, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	v, err := s.repo.Get(ctx, r.TenantId, r.VmId)
	if err != nil {
		return nil, rpcError(err)
	}
	i, err := biz.FindSnapshot(v, r.SnapshotId)
	if err != nil {
		return nil, rpcError(err)
	}
	item, err := s.snapshotObservation(ctx, v, i)
	if err != nil {
		return nil, rpcError(err)
	}
	return snapshotWire(item), nil
}
func (s *Service) CreateSnapshot(ctx context.Context, r *pb.SnapshotCreateRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "create-snapshot")
}
func (s *Service) DeleteSnapshot(ctx context.Context, r *pb.SnapshotChangeRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "delete-snapshot")
}
func (s *Service) RestoreSnapshot(ctx context.Context, r *pb.RestoreSnapshotRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "restore-snapshot")
}
func (s *Service) PreviewRestore(ctx context.Context, r *pb.PreviewRestoreRequest) (*pb.RestorePreview, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	v, err := s.repo.Get(ctx, r.TenantId, r.VmId)
	if err != nil {
		return nil, rpcError(err)
	}
	i, err := biz.FindSnapshot(v, r.SnapshotId)
	if err != nil {
		return nil, rpcError(err)
	}
	item, err := s.snapshotObservation(ctx, v, i)
	if err != nil {
		return nil, rpcError(err)
	}
	if err = s.restoreTargetStopped(ctx, v); err != nil {
		return nil, rpcError(err)
	}
	plan, err := biz.PreviewSnapshotRestore(v, item)
	if err != nil {
		return nil, rpcError(err)
	}
	result := &pb.RestorePreview{Version: plan.Version, SnapshotId: plan.SnapshotID, SnapshotUid: plan.SnapshotUID, PreviewDigest: plan.Digest}
	for _, disk := range plan.Impacts {
		result.Disks = append(result.Disks, &pb.RestoreDiskImpact{DiskId: disk.DiskID, Action: disk.Action, Reason: disk.Reason})
	}
	return result, nil
}
func (s *Service) restoreTargetStopped(ctx context.Context, v biz.VM) error {
	observed, err := s.native.Observe(ctx, v)
	if err != nil {
		return err
	}
	if observed.VM.UID != v.Native.UID || v.Native.UID == "" {
		return biz.ErrOwnership
	}
	if !observed.Stopped || observed.VMI != nil || observed.Launcher != nil {
		return biz.ErrBusy
	}
	return nil
}

// Called only after current authorization and read-only replay lookup. Native
// checks precede acceptance; the pure validator runs again under the VM row lock.
func (s *Service) validateSnapshotCommand(ctx context.Context, c biz.Command) error {
	if c.Action != "create-snapshot" && c.Action != "delete-snapshot" && c.Action != "restore-snapshot" {
		return nil
	}
	v, err := s.repo.Get(ctx, c.TenantID, c.VMID)
	if err != nil {
		return err
	}
	if v.Version != c.ExpectedVersion {
		return biz.ErrVersion
	}
	if v.Closing {
		return biz.ErrBusy
	}
	if err = biz.ValidateSnapshotCommand(v, c.Action, c.Intent); err != nil {
		return err
	}
	native, ok := s.native.(biz.SnapshotNativePort)
	if !ok {
		return biz.ErrDependency
	}
	if c.Action == "create-snapshot" {
		observed, err := s.native.Observe(ctx, v)
		if err != nil {
			return err
		}
		if observed.VM.UID != v.Native.UID {
			return biz.ErrOwnership
		}
		for i := range v.Disks {
			for _, disk := range observed.Volumes {
				if v.Disks[i].ID == disk.ID {
					v.Disks[i] = disk
				}
			}
		}
		item, err := biz.NewSnapshot(v, v.ID, "snapshot-capability-check")
		if err != nil {
			return err
		}
		_, err = native.PrepareSnapshot(ctx, v, item)
		return err
	}
	var request struct {
		SnapshotID    string `json:"snapshot_id"`
		PreviewDigest string `json:"preview_digest"`
	}
	if err = json.Unmarshal(c.Intent, &request); err != nil {
		return biz.ErrInvalid
	}
	index, err := biz.FindSnapshot(v, request.SnapshotID)
	if err != nil {
		return err
	}
	fresh, err := s.snapshotObservation(ctx, v, index)
	if err != nil {
		return err
	}
	if c.Action == "restore-snapshot" {
		if err = s.restoreTargetStopped(ctx, v); err != nil {
			return err
		}
		plan, err := biz.PreviewSnapshotRestore(v, fresh)
		if err != nil {
			return err
		}
		if plan.Digest != request.PreviewDigest {
			return biz.ErrChanged
		}
	}
	return nil
}
