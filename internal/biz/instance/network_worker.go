package instance

import (
	"context"
	"errors"
)

func (w *Worker) UseNetwork(network NetworkPort) error {
	if network == nil {
		return ErrInvalid
	}
	w.network = network
	return nil
}

// prepareNetwork is a durable handoff: the send marker is committed in one
// turn; the following turn may issue the stable Prepare request. Any lost
// receipt is recovered using the same identity, including after Delete.
func (w *Worker) prepareNetwork(ctx context.Context, i *Instance) (bool, error) {
	if i.Network == nil || i.Network.Cleaned || i.Network.AttachmentID != "" {
		return false, nil
	}
	n := i.Network
	if !n.PrepareStarted {
		n.PrepareStarted = true
		n.Reason = "NETWORK_PREPARE_PENDING"
		return true, nil
	}
	if w.network == nil {
		n.Reason = "INSTANCE_NETWORK_UNAVAILABLE"
		return true, nil
	}
	binding, err := w.network.Prepare(ctx, *i)
	if err != nil {
		n.Reason = networkReason(err)
		return true, nil
	}
	n.AttachmentID = binding.AttachmentID
	n.BindingRevision = binding.BindingRevision
	n.PrimaryNetworkRef = binding.PrimaryNetworkRef
	n.AttachmentVersion = binding.Version
	n.NetworkState = "reserved"
	n.Reason = ""
	return true, nil
}
func networkReason(err error) string {
	switch {
	case errors.Is(err, ErrNetworkNotFound):
		return "INSTANCE_NETWORK_NOT_FOUND"
	case errors.Is(err, ErrNetworkConflict):
		return "INSTANCE_NETWORK_CONFLICT"
	default:
		return "INSTANCE_NETWORK_UNAVAILABLE"
	}
}
func (w *Worker) observeNetwork(ctx context.Context, i *Instance) bool {
	if i.Network == nil {
		return true
	}
	n := i.Network
	if n.Cleaned {
		return false
	}
	if w.network == nil || n.AttachmentID == "" {
		n.Reason = "INSTANCE_NETWORK_UNAVAILABLE"
		return false
	}
	o, err := w.network.Observe(ctx, *i)
	if err != nil {
		n.Reason = networkReason(err)
		return false
	}
	n.AttachmentVersion = o.Version
	n.NetworkState = o.State
	n.Reason = o.Reason
	if n.State == "closed" && n.ReleaseAcknowledged && o.State == "released" && o.Reason == "" {
		n.Cleaned = true
		n.Reason = ""
		return false
	}
	return i.RuntimeDesired == Running && n.State == "open" && o.State == "attached" && o.Reason == "" && o.PodUID != "" && o.PodUID == i.PodUID && o.SnapshotRevision == n.SnapshotRevision && n.ConfirmedSnapshotRevision == n.SnapshotRevision
}

// Confirm only a revision already published by a successful Instance Finish.
// Version conflicts are normal races with Network's observer and remain retryable.
func (w *Worker) confirmNetwork(ctx context.Context, i *Instance) (bool, error) {
	n := i.Network
	if n == nil || n.State != "open" || i.RuntimeDesired != Running || i.PodUID == "" || n.ConfirmedSnapshotRevision == n.SnapshotRevision {
		return false, nil
	}
	if w.network == nil || n.AttachmentID == "" {
		return false, nil
	}
	if err := w.network.Confirm(ctx, *i); err != nil {
		n.Reason = networkReason(err)
		return false, nil
	}
	n.ConfirmedSnapshotRevision = n.SnapshotRevision
	return true, nil
}
func (w *Worker) releaseNetwork(ctx context.Context, i *Instance) bool {
	n := i.Network
	if n == nil || n.Cleaned {
		return true
	}
	if n.State != "closed" {
		n.Reason = "WAITING_FOR_NETWORK_SUBMISSION_CLOSURE"
		return false
	}
	if w.network == nil || n.AttachmentID == "" {
		n.Reason = "INSTANCE_NETWORK_UNAVAILABLE"
		return false
	}
	if !n.ReleaseStarted {
		n.ReleaseStarted = true
		n.Reason = "NETWORK_RELEASE_PENDING"
		return false
	}
	if !n.ReleaseAcknowledged {
		if err := w.network.Release(ctx, *i); err != nil {
			n.Reason = networkReason(err)
			return false
		}
		n.ReleaseAcknowledged = true
		n.Reason = "WAITING_FOR_NETWORK_RELEASE"
	}
	if n.NetworkState == "released" && n.ReleaseAcknowledged {
		n.Cleaned = true
		n.Reason = ""
		return true
	}
	return false
}
