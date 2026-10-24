package vm

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Worker struct {
	repo    Repository
	native  NativePort
	network NetworkPort
}

func NewWorker(r Repository, n NativePort, network NetworkPort) (*Worker, error) {
	if r == nil || n == nil || network == nil {
		return nil, ErrInvalid
	}
	return &Worker{r, n, network}, nil
}
func (w *Worker) RunOnce(ctx context.Context) error {
	o, v, e := w.repo.Claim(ctx, uuid.NewString(), 30*time.Second)
	if e != nil {
		return e
	}
	e = w.advance(ctx, &o, &v)
	if e != nil {
		o.Reason = e.Error()
		if errors.Is(e, ErrUnknown) {
			o.State = "unknown"
		} else if errors.Is(e, ErrOwnership) || errors.Is(e, ErrInvalid) || errors.Is(e, ErrChanged) || errors.Is(e, ErrStartFailed) {
			o.State = "failed"
		} else {
			o.State = "running"
		}
	} else {
		o.Reason = ""
	}
	x := w.repo.Save(ctx, o, v)
	if x != nil {
		if errors.Is(x, ErrVersion) || errors.Is(x, ErrLease) {
			// A newer intent or lease already owns this work. Its durable
			// state wins; losing the fence does not stop other VM workers.
			return nil
		}
		return x
	}
	// Native reconciliation failures are now durable operation outcomes.
	// Returning them to the service supervisor would terminate the shared
	// service whenever a dependency is still preparing or a result is unknown.
	return nil
}
func (w *Worker) Step(ctx context.Context) (bool, error) {
	e := w.RunOnce(ctx)
	if errors.Is(e, ErrNotFound) {
		return false, nil
	}
	return true, e
}
func (w *Worker) checkpoint(ctx context.Context, o *Operation, v VM, action string, target ObjectRef) error {
	o.Pending = &Mutation{Action: action, Target: target, Submitted: true}
	return w.repo.Checkpoint(ctx, *o, v)
}
func (w *Worker) advance(ctx context.Context, o *Operation, v *VM) error {
	if o.Action == DeleteAction {
		return w.remove(ctx, o, v)
	}
	if v.Closing {
		return ErrBusy
	}
	if o.Action == CreateAction {
		return w.create(ctx, o, v)
	}
	switch o.Action {
	case "start", "stop", "force-stop", "restart":
		return w.power(ctx, o, v)
	case "create-disk", "attach-disk", "detach-disk", "delete-disk":
		return w.disk(ctx, o, v)
	case "set-media", "eject-media", "set-boot":
		return w.media(ctx, o, v)
	case "create-snapshot", "delete-snapshot", "restore-snapshot":
		return w.snapshot(ctx, o, v)
	default:
		return ErrDependency
	}
}
func (w *Worker) create(ctx context.Context, o *Operation, v *VM) error {
	if e := InitializeMedia(v); e != nil {
		return e
	}
	if v.Network.AttachmentID == "" {
		if e := w.checkpoint(ctx, o, *v, "prepare-network", ObjectRef{}); e != nil {
			return e
		}
		plan, e := w.network.Prepare(ctx, *v)
		if e != nil {
			return e
		}
		v.Network = plan
		o.Pending = nil
		return nil
	}
	if v.Native.UID == "" {
		if e := w.native.ValidateSource(ctx, v.Spec.Media); e != nil {
			return e
		}
		if e := w.checkpoint(ctx, o, *v, "create-vm", v.Native); e != nil {
			return e
		}
		ref, e := w.native.EnsureVM(ctx, *v)
		if e != nil {
			return e
		}
		v.Native = ref
		o.Pending = nil
		return nil
	}
	if v.Spec.GuestInitialization != nil && v.InitializationSecret.UID == "" {
		if e := w.checkpoint(ctx, o, *v, "create-initialization-secret", v.InitializationSecret); e != nil {
			return e
		}
		ref, e := w.native.EnsureInitialization(ctx, *v)
		if e != nil {
			return e
		}
		v.InitializationSecret = ref
		o.Pending = nil
		return nil
	}
	if changed, e := w.ensureDesiredMediaCopies(ctx, o, v); e != nil || changed {
		return e
	}
	for i := range v.Disks {
		if v.Disks[i].DV.UID == "" {
			if v.Disks[i].Role == "system" && v.Spec.Media.Kind == "DISK_IMAGE" {
				if e := w.native.ValidateSource(ctx, v.Spec.Media); e != nil {
					return e
				}
			}
			if e := w.checkpoint(ctx, o, *v, "create-volume", v.Disks[i].DV); e != nil {
				return e
			}
			d, e := w.native.EnsureVolume(ctx, *v, v.Disks[i])
			if e != nil {
				return e
			}
			v.Disks[i] = d
			o.Pending = nil
			return nil
		}
	}
	if !v.Spec.StartAfterCreate {
		o.State = "succeeded"
		v.Observed = "Stopped"
		v.ObservedAt = time.Now().UTC()
		return nil
	}
	return w.power(ctx, o, v)
}
func (w *Worker) power(ctx context.Context, o *Operation, v *VM) error {
	x, e := w.native.Observe(ctx, *v)
	if e != nil {
		return e
	}
	v.Observed = x.State
	v.Reason = x.Reason
	v.ObservedAt = time.Now().UTC()
	v.BackendStates = x.BackendStates
	if e = w.observeMedia(ctx, v); e != nil {
		return e
	}
	if x.VM.UID != "" && v.Native.UID != x.VM.UID {
		return ErrOwnership
	}
	if len(x.Volumes) > 0 {
		for i := range v.Disks {
			for _, d := range x.Volumes {
				if v.Disks[i].ID == d.ID {
					v.Disks[i] = d
				}
			}
		}
	}
	addresses, e := w.network.Observe(ctx, *v, x)
	if e != nil && !errors.Is(e, ErrDependency) {
		return e
	}
	v.Addresses = addresses
	action := o.Action
	if action == CreateAction {
		action = "start"
	}
	switch action {
	case "start":
		v.Desired = "Running"
		if x.Running {
			o.State = "succeeded"
			o.Pending = nil
			return nil
		}
		// Only a VMI different from the identity observed before this Start can
		// prove its terminal failure. Legacy checkpoints lack that observation;
		// an unchanged old VMI or an outstanding native request remains unknown.
		if o.Pending != nil && o.Pending.Action == "start" && o.Pending.Submitted && o.Pending.Target == v.Native &&
			x.State == "Failed" && x.VMI != nil && x.VMI.UID != "" && v.Native.UID != "" && x.VM.UID == v.Native.UID &&
			o.Pending.BeforeVMIUID != nil && x.VMI.UID != *o.Pending.BeforeVMIUID &&
			len(x.StateChangeRequests) == 0 {
			o.Pending = nil
			v.Reason = ErrStartFailed.Error()
			return ErrStartFailed
		}
		// Start converges to one VMI for this VM. After observing an actually
		// stopped VM with no VMI, launcher or native request, retrying Start is
		// safe. Restart remains fenced below because it creates a new VMI.
		if o.Pending != nil && o.Pending.Submitted && !(o.Pending.Action == "start" && x.Stopped) {
			return ErrUnknown
		}
		beforeVMIUID := ""
		if x.VMI != nil {
			beforeVMIUID = x.VMI.UID
		}
		o.Pending = &Mutation{Action: "start", Target: v.Native, Submitted: true, BeforeVMIUID: &beforeVMIUID}
		if e = w.repo.Checkpoint(ctx, *o, *v); e != nil {
			return e
		}
		return w.native.Start(ctx, *v)
	case "stop", "force-stop":
		v.Desired = "Stopped"
		if x.Stopped && x.VMI == nil && x.Launcher == nil && len(v.Addresses) == 0 {
			o.State = "succeeded"
			o.Pending = nil
			return nil
		}
		if o.Pending != nil && o.Pending.Submitted {
			return ErrUnknown
		}
		if e = w.checkpoint(ctx, o, *v, action, v.Native); e != nil {
			return e
		}
		return w.native.Stop(ctx, *v, action == "force-stop")
	case "restart":
		v.Desired = "Running"
		if o.OldVMIUID != "" && x.VMI != nil && x.VMI.UID != o.OldVMIUID && x.Running {
			o.State = "succeeded"
			o.Pending = nil
			return nil
		}
		if o.Pending != nil && o.Pending.Submitted {
			return ErrUnknown
		}
		if x.VMI == nil || !x.Running {
			return ErrDependency
		}
		o.OldVMIUID = x.VMI.UID
		if e = w.checkpoint(ctx, o, *v, "restart", *x.VMI); e != nil {
			return e
		}
		return w.native.Restart(ctx, *v)
	}
	return ErrInvalid
}
func (w *Worker) remove(ctx context.Context, o *Operation, v *VM) error {
	if !v.Closing {
		return ErrBusy
	}
	if done, e := w.removeSnapshots(ctx, o, v); e != nil || !done {
		return e
	}
	if e := InitializeMedia(v); e != nil {
		return e
	}
	if e := w.observeMedia(ctx, v); e != nil {
		return e
	}
	x, e := w.native.Observe(ctx, *v)
	if e != nil {
		return e
	}
	v.BackendStates = x.BackendStates
	for i := range v.Disks {
		for _, d := range x.Volumes {
			if v.Disks[i].ID == d.ID {
				v.Disks[i] = d
			}
		}
	}
	if !x.Deleted {
		// Unknown creation is resolved by the frozen name and ownership before
		// deletion. An absent root never authorizes deleting a new same-name UID.
		if v.Native.UID == "" && x.VM.UID != "" {
			v.Native = x.VM
			return nil
		}
		if e = w.checkpoint(ctx, o, *v, "delete-vm", v.Native); e != nil {
			return e
		}
		return w.native.Delete(ctx, *v)
	}
	v.Addresses = nil
	if v.Spec.GuestInitialization != nil {
		if e = w.checkpoint(ctx, o, *v, "delete-initialization-secret", v.InitializationSecret); e != nil {
			return e
		}
		done, e := w.native.DeleteInitialization(ctx, *v)
		if e != nil {
			return e
		}
		if !done {
			return nil
		}
		v.InitializationSecret = ObjectRef{}
	}
	for i := range v.Disks {
		if v.Disks[i].Tombstone {
			continue
		}
		if e = w.checkpoint(ctx, o, *v, "delete-volume", v.Disks[i].DV); e != nil {
			return e
		}
		done, x := w.native.DeleteVolume(ctx, *v, v.Disks[i])
		if x != nil {
			return x
		}
		if !done {
			return nil
		}
		v.Disks[i].Tombstone = true
		return nil
	}
	if done, e := w.removeMedia(ctx, o, v); e != nil || !done {
		return e
	}
	if done, e := w.removeRetiredVolumes(ctx, o, v); e != nil || !done {
		return e
	}
	if len(v.BackendStates) != 0 {
		native, ok := w.native.(BackendNativePort)
		if !ok {
			return ErrDependency
		}
		for _, backend := range v.BackendStates {
			done, e := native.BackendStateRemoved(ctx, *v, backend)
			if e != nil {
				return e
			}
			if !done {
				return ErrDependency
			}
		}
	}
	if len(v.Media) > 0 || len(v.Cleanup) > 0 {
		return ErrDependency
	}
	if e = w.checkpoint(ctx, o, *v, "release-network", ObjectRef{}); e != nil {
		return e
	}
	done, e := w.network.Release(ctx, *v)
	if e != nil {
		return e
	}
	if !done {
		return nil
	}
	o.State = "succeeded"
	o.Pending = nil
	v.Observed = "Deleted"
	v.Reason = ""
	v.ObservedAt = time.Now().UTC()
	return nil
}
func NewInitialDisk(vmID, key, name, role string, size int64) Disk {
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("ani-vm-disk/"+vmID+"/"+role+"/"+key)).String()
	return Disk{ID: id, Role: role, Name: name, SizeGB: size, RequestedBytes: size * 1000000000, VolumeName: "disk-" + strings.ReplaceAll(id, "-", ""), Serial: id, Attached: true, DV: ObjectRef{Kind: "DataVolume", Name: "vd-" + id}, Preparation: "PendingPopulation"}
}
