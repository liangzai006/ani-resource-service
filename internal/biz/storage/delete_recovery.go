package storage

import (
	"context"
	"errors"
	"time"
)

const deleteLeaseTTL = 45 * time.Second
const deleteRetryInterval = 5 * time.Second

func (u *UseCase) deleteGuardReady() bool {
	return u.cfg.BorrowedPVCGuard != nil && CanonicalUUID(u.cfg.ClusterID) && u.cfg.DeleteExecutor != ""
}

func (u *UseCase) deleteGet(ctx context.Context, namespace, name string) (*VolumeRecord, error) {
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return u.volumes.GetVolume(call, namespace, name)
}

func (u *UseCase) deleteNamespace(ctx context.Context, tenant string) (string, error) {
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return u.ensureNamespace(call, tenant)
}

func (u *UseCase) deleteVolumeGuarded(ctx context.Context, tenant, name, expectedUID string) (DeleteResult, error) {
	if !CanonicalUUID(tenant) || name == "" || len(name) > 253 || !dnsSubdomain.MatchString(name) || expectedUID == "" {
		return DeleteResult{}, ErrInvalid
	}
	// No guard means no fallback to a direct Kubernetes deletion.
	if !u.deleteGuardReady() {
		return DeleteResult{}, ErrBackendUnavailable
	}
	step, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	namespace, err := u.deleteNamespace(step, tenant)
	if err != nil {
		return DeleteResult{}, err
	}
	current, getErr := u.deleteGet(step, namespace, name)
	if getErr != nil && !errors.Is(getErr, ErrNotFound) {
		return DeleteResult{}, getErr
	}
	if getErr == nil {
		if !owned(current, tenant) {
			return DeleteResult{}, ErrNotFound
		}
		if current.UID != expectedUID {
			// An existing old-UID ticket remains protected and is completed by
			// recovery; the public call never adopts the same-name new object.
			return DeleteResult{}, ErrIdentityChanged
		}
	}
	id := PVCIdentity{TenantID: tenant, ClusterID: u.cfg.ClusterID, Namespace: namespace, PVCName: name, TargetUID: expectedUID}
	record, mayAdvance, err := u.cfg.BorrowedPVCGuard.ReserveDelete(step, id, u.cfg.DeleteExecutor, deleteLeaseTTL)
	if err != nil {
		return DeleteResult{}, deleteGuardError(err)
	}
	if record.Identity != id {
		return DeleteResult{}, ErrIdentityChanged
	}
	if mayAdvance {
		return u.advanceDelete(step, record)
	}
	// A retry cannot steal a valid lease. Only already known results may be
	// returned without advancing; otherwise it waits for that writer/recovery.
	if errors.Is(getErr, ErrNotFound) {
		return DeleteResult{State: DeletionGone}, nil
	}
	if record.Phase == "completed" {
		return DeleteResult{}, ErrBackendUnavailable
	}
	if current.Deleting {
		return DeleteResult{State: DeletionTerminating, Volume: toVolume(current)}, nil
	}
	if record.WriteStarted && record.LastWriteOutcome == "submitted" {
		return DeleteResult{State: DeletionSubmitted, Volume: toVolume(current)}, nil
	}
	return DeleteResult{}, ErrBackendUnavailable
}

func deleteGuardError(err error) error {
	if errors.Is(err, ErrVolumeInUse) || errors.Is(err, ErrIdentityChanged) || errors.Is(err, ErrNotFound) {
		return err
	}
	return ErrBackendUnavailable
}

func (u *UseCase) validDeleteRecord(record DeleteRecord) bool {
	id, token := record.Identity, record.Token
	namespace, err := u.namespace(id.TenantID)
	return err == nil && id.ClusterID == u.cfg.ClusterID && id.Namespace == namespace && id.PVCName != "" && id.TargetUID != "" &&
		token.TicketID != "" && token.GuardEpoch > 0 && token.LeaseOwner == u.cfg.DeleteExecutor && token.LeaseEpoch > 0 && token.LeaseUntil.After(u.now()) &&
		(record.Phase == "reserved" || record.Phase == "deleting" || record.Phase == "observing")
}

func (u *UseCase) recordDeleteWait(ctx context.Context, record DeleteRecord, observation DeleteObservation, cause error) (DeleteResult, error) {
	observation.RetryAt = u.now().Add(deleteRetryInterval)
	if err := u.cfg.BorrowedPVCGuard.RecordDeleteObservation(ctx, record.Identity, record.Token, observation); err != nil {
		return DeleteResult{}, ErrBackendUnavailable
	}
	if cause != nil {
		return DeleteResult{}, cause
	}
	return DeleteResult{}, ErrBackendUnavailable
}

// advanceDelete is the only finite advancement path, shared by HTTP and
// recovery. It commits the possible-write marker before the UID-bound DELETE.
func (u *UseCase) advanceDelete(ctx context.Context, record DeleteRecord) (DeleteResult, error) {
	if !u.validDeleteRecord(record) {
		return DeleteResult{}, ErrBackendUnavailable
	}
	id := record.Identity
	namespace, err := u.deleteNamespace(ctx, id.TenantID)
	if err != nil || namespace != id.Namespace {
		return u.recordDeleteWait(ctx, record, DeleteObservation{State: "unavailable", WriteOutcome: record.LastWriteOutcome, Reason: "NAMESPACE_UNAVAILABLE"}, ErrBackendUnavailable)
	}
	current, err := u.deleteGet(ctx, id.Namespace, id.PVCName)
	if errors.Is(err, ErrNotFound) {
		return u.completeDelete(ctx, record, DeleteObservation{State: "absent", WriteOutcome: record.LastWriteOutcome})
	}
	if err != nil {
		return u.recordDeleteWait(ctx, record, DeleteObservation{State: "unavailable", WriteOutcome: record.LastWriteOutcome, Reason: "PVC_OBSERVATION_UNAVAILABLE"}, ErrBackendUnavailable)
	}
	if current.UID != id.TargetUID {
		return u.completeDelete(ctx, record, DeleteObservation{State: "identity_changed", ObservedUID: current.UID, WriteOutcome: record.LastWriteOutcome})
	}
	if !owned(current, id.TenantID) {
		if !record.WriteStarted {
			if err := u.cfg.BorrowedPVCGuard.CancelUnsentDelete(ctx, id, record.Token, "PVC_OWNERSHIP_CHANGED"); err != nil {
				return DeleteResult{}, ErrBackendUnavailable
			}
			return DeleteResult{}, ErrNotFound
		}
		return u.recordDeleteWait(ctx, record, DeleteObservation{State: "present", ObservedUID: current.UID, WriteOutcome: record.LastWriteOutcome, Reason: "PVC_OWNERSHIP_CHANGED"}, ErrBackendUnavailable)
	}
	if !record.WriteStarted {
		// Reserved recovery must repeat the complete external usage check.
		// Once a write may have been sent, usage cannot cancel its barrier.
		consumers, err := u.deleteUsage(ctx, id.Namespace, id.PVCName)
		if err != nil {
			return u.recordDeleteWait(ctx, record, DeleteObservation{State: "present", ObservedUID: current.UID, WriteOutcome: "not_sent", Reason: "VOLUME_USAGE_UNKNOWN"}, ErrUsageUnknown)
		}
		if len(consumers) > 0 {
			if err := u.cfg.BorrowedPVCGuard.CancelUnsentDelete(ctx, id, record.Token, "VOLUME_IN_USE"); err != nil {
				return DeleteResult{}, ErrBackendUnavailable
			}
			return DeleteResult{}, &InUseError{Consumers: consumers}
		}
	}
	if err := u.cfg.BorrowedPVCGuard.MarkDeleteWriteStarted(ctx, id, record.Token); err != nil {
		return DeleteResult{}, ErrBackendUnavailable
	}
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	writeErr := u.volumes.DeleteVolume(call, id.Namespace, id.PVCName, id.TargetUID)
	cancel()
	outcome, reason := "submitted", ""
	if writeErr != nil && !errors.Is(writeErr, ErrNotFound) {
		outcome, reason = "unknown", "PVC_DELETE_RESULT_UNKNOWN"
		if errors.Is(writeErr, ErrAccessDenied) || errors.Is(writeErr, ErrConflict) || errors.Is(writeErr, ErrIdentityChanged) {
			outcome, reason = "rejected", "PVC_DELETE_REJECTED"
		}
	}
	if u.cfg.ObserveDeleteWrite != nil {
		u.cfg.ObserveDeleteWrite(outcome)
	}
	current, err = u.deleteGet(ctx, id.Namespace, id.PVCName)
	if errors.Is(err, ErrNotFound) {
		return u.completeDelete(ctx, record, DeleteObservation{State: "absent", WriteOutcome: outcome})
	}
	if err != nil {
		return u.recordDeleteWait(ctx, record, DeleteObservation{State: "unavailable", WriteOutcome: outcome, Reason: "PVC_OBSERVATION_UNAVAILABLE"}, ErrBackendUnavailable)
	}
	if current.UID != id.TargetUID {
		return u.completeDelete(ctx, record, DeleteObservation{State: "identity_changed", ObservedUID: current.UID, WriteOutcome: outcome})
	}
	state, publicState := "present", DeletionSubmitted
	if current.Deleting {
		state, publicState = "terminating", DeletionTerminating
	}
	observation := DeleteObservation{State: state, ObservedUID: current.UID, WriteOutcome: outcome, Reason: reason, RetryAt: u.now().Add(deleteRetryInterval)}
	if err := u.cfg.BorrowedPVCGuard.RecordDeleteObservation(ctx, id, record.Token, observation); err != nil {
		return DeleteResult{}, ErrBackendUnavailable
	}
	if writeErr != nil && !current.Deleting {
		return DeleteResult{}, ErrBackendUnavailable
	}
	return DeleteResult{State: publicState, Volume: toVolume(current)}, nil
}

func (u *UseCase) completeDelete(ctx context.Context, record DeleteRecord, observation DeleteObservation) (DeleteResult, error) {
	if err := u.cfg.BorrowedPVCGuard.CompleteDelete(ctx, record.Identity, record.Token, observation); err != nil {
		return DeleteResult{}, ErrBackendUnavailable
	}
	if observation.State == "identity_changed" {
		return DeleteResult{}, ErrIdentityChanged
	}
	return DeleteResult{State: DeletionGone}, nil
}

func (u *UseCase) deleteUsage(ctx context.Context, namespace, name string) ([]Consumer, error) {
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	pods, err := u.volumes.ListPodConsumers(call, namespace, name)
	cancel()
	if err != nil {
		return nil, ErrUsageUnknown
	}
	call, cancel = context.WithTimeout(ctx, 15*time.Second)
	notebooks, err := u.volumes.ListNotebookConsumers(call, namespace, name)
	cancel()
	if err != nil && !errors.Is(err, ErrNotInstalled) {
		return nil, ErrUsageUnknown
	}
	return append(pods, notebooks...), nil
}

func (u *UseCase) StepDeleteRecovery(ctx context.Context) (bool, error) {
	if !u.deleteGuardReady() {
		return false, ErrBackendUnavailable
	}
	step, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	record, err := u.cfg.BorrowedPVCGuard.ClaimDeleteRecovery(step, u.cfg.DeleteExecutor, deleteLeaseTTL)
	if err != nil || record == nil {
		return false, err
	}
	_, err = u.advanceDelete(step, *record)
	return true, err
}

// The composition root owns this goroutine's start, cancellation and join.
// Per-ticket errors remain durable; a failed ticket never kills recovery.
func (u *UseCase) RunDeleteRecovery(ctx context.Context) error {
	if !u.deleteGuardReady() {
		return ErrBackendUnavailable
	}
	ticker := time.NewTicker(deleteRetryInterval)
	defer ticker.Stop()
	for {
		_, _ = u.StepDeleteRecovery(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
