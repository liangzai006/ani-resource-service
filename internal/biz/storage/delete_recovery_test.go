package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// This is only the owner port's boundary double. SQL/lease/guard concurrency is
// exercised separately against the real Instance repository and PostgreSQL.
type testDeleteGuard struct {
	records                   map[string]DeleteRecord
	reserveErr                error
	markErr                   error
	reserveCalls, markCalls   int
	recordCalls, cancelCalls  int
	completeCalls, claimCalls int
}

func newTestDeleteGuard() *testDeleteGuard {
	return &testDeleteGuard{records: map[string]DeleteRecord{}}
}
func deleteTestKey(id PVCIdentity) string {
	return id.ClusterID + "/" + id.Namespace + "/" + id.PVCName
}

func (g *testDeleteGuard) ReserveDelete(_ context.Context, id PVCIdentity, owner string, ttl time.Duration) (DeleteRecord, bool, error) {
	g.reserveCalls++
	if g.reserveErr != nil {
		return DeleteRecord{}, false, g.reserveErr
	}
	key := deleteTestKey(id)
	record, exists := g.records[key]
	if exists && record.Phase != "cancelled" {
		if record.Identity != id {
			return record, false, ErrIdentityChanged
		}
		if record.Phase == "completed" || record.Token.LeaseUntil.After(time.Now()) {
			return record, false, nil
		}
	} else {
		epoch := record.Token.GuardEpoch + 1
		record = DeleteRecord{Identity: id, Phase: "reserved", Token: DeleteToken{TicketID: "ticket-" + id.PVCName, GuardEpoch: epoch}}
	}
	record.Token.LeaseOwner, record.Token.LeaseUntil = owner, time.Now().Add(ttl)
	record.Token.LeaseEpoch++
	g.records[key] = record
	return record, true, nil
}

func (g *testDeleteGuard) ClaimDeleteRecovery(ctx context.Context, owner string, ttl time.Duration) (*DeleteRecord, error) {
	g.claimCalls++
	for _, record := range g.records {
		if record.Phase == "completed" || record.Phase == "cancelled" || record.Token.LeaseUntil.After(time.Now()) {
			continue
		}
		record, acquired, err := g.ReserveDelete(ctx, record.Identity, owner, ttl)
		if err != nil || !acquired {
			return nil, err
		}
		return &record, nil
	}
	return nil, nil
}

func (g *testDeleteGuard) check(id PVCIdentity, token DeleteToken) (DeleteRecord, error) {
	record, exists := g.records[deleteTestKey(id)]
	if !exists || record.Identity != id || !reflect.DeepEqual(record.Token, token) || !token.LeaseUntil.After(time.Now()) {
		return record, ErrDeleteLease
	}
	return record, nil
}

func (g *testDeleteGuard) MarkDeleteWriteStarted(_ context.Context, id PVCIdentity, token DeleteToken) error {
	g.markCalls++
	if g.markErr != nil {
		return g.markErr
	}
	record, err := g.check(id, token)
	if err != nil {
		return err
	}
	record.WriteStarted, record.Phase = true, "deleting"
	g.records[deleteTestKey(id)] = record
	return nil
}

func (g *testDeleteGuard) RecordDeleteObservation(_ context.Context, id PVCIdentity, token DeleteToken, observation DeleteObservation) error {
	g.recordCalls++
	record, err := g.check(id, token)
	if err != nil {
		return err
	}
	record.LastObservation, record.LastWriteOutcome, record.LastReason, record.ObservedUID = observation.State, observation.WriteOutcome, observation.Reason, observation.ObservedUID
	record.Token.LeaseUntil = time.Time{}
	if record.WriteStarted {
		record.Phase = "observing"
	}
	g.records[deleteTestKey(id)] = record
	return nil
}

func (g *testDeleteGuard) CancelUnsentDelete(_ context.Context, id PVCIdentity, token DeleteToken, _ string) error {
	g.cancelCalls++
	record, err := g.check(id, token)
	if err != nil || record.WriteStarted {
		return ErrDeleteLease
	}
	record.Phase, record.Token.LeaseUntil = "cancelled", time.Time{}
	g.records[deleteTestKey(id)] = record
	return nil
}

func (g *testDeleteGuard) CompleteDelete(_ context.Context, id PVCIdentity, token DeleteToken, observation DeleteObservation) error {
	g.completeCalls++
	record, err := g.check(id, token)
	if err != nil {
		return err
	}
	if observation.State != "absent" && observation.State != "identity_changed" {
		return ErrInvalid
	}
	record.Phase, record.Token.LeaseUntil = "completed", time.Time{}
	record.LastObservation, record.LastWriteOutcome, record.ObservedUID = observation.State, observation.WriteOutcome, observation.ObservedUID
	g.records[deleteTestKey(id)] = record
	return nil
}

type deleteBoundaryVolumes struct {
	*fakeVolumes
	deleteCalls []string
	onDelete    func(string, string) error
}

func (v *deleteBoundaryVolumes) DeleteVolume(ctx context.Context, namespace, name, uid string) error {
	v.deleteCalls = append(v.deleteCalls, uid)
	if v.onDelete != nil {
		return v.onDelete(name, uid)
	}
	return v.fakeVolumes.DeleteVolume(ctx, namespace, name, uid)
}

func guardedDeleteFixture(t *testing.T) (*UseCase, *deleteBoundaryVolumes, *testDeleteGuard, PVCIdentity) {
	t.Helper()
	volumes := &deleteBoundaryVolumes{fakeVolumes: newFake()}
	guard := newTestDeleteGuard()
	u, err := NewUseCase(volumes, nil, Config{NamespacePrefix: testPrefix, ClusterID: testTenant, DeleteExecutor: "test-storage", BorrowedPVCGuard: guard})
	if err != nil {
		t.Fatal(err)
	}
	name, uid := "data", "original-pvc-uid"
	volumes.records[name] = &VolumeRecord{Name: name, UID: uid, Labels: map[string]string{LabelManagedBy: ManagedByValue, LabelTenantID: testTenant}}
	return u, volumes, guard, PVCIdentity{TenantID: testTenant, ClusterID: testTenant, Namespace: testNS, PVCName: name, TargetUID: uid}
}

func releaseTestDeleteLease(g *testDeleteGuard, id PVCIdentity) {
	record := g.records[deleteTestKey(id)]
	record.Token.LeaseUntil = time.Time{}
	g.records[deleteTestKey(id)] = record
}

func TestGuardedDeleteMissingOrUnavailableGuardNeverFallsBack(t *testing.T) {
	u, volumes, guard, id := guardedDeleteFixture(t)
	u.cfg.BorrowedPVCGuard = nil
	if _, err := u.DeleteVolume(context.Background(), id.TenantID, id.PVCName, id.TargetUID); !errors.Is(err, ErrBackendUnavailable) || len(volumes.deleteCalls) != 0 {
		t.Fatal("missing guard fell back to Kubernetes", err)
	}
	u.cfg.BorrowedPVCGuard = guard
	guard.reserveErr = errors.New("owner database unavailable")
	if _, err := u.DeleteVolume(context.Background(), id.TenantID, id.PVCName, id.TargetUID); !errors.Is(err, ErrBackendUnavailable) || len(volumes.deleteCalls) != 0 {
		t.Fatal("unavailable guard fell back to Kubernetes", err)
	}
}

func TestGuardedDeleteCommitsMarkerBeforeUIDDeleteAndCompletesDirectly(t *testing.T) {
	u, volumes, guard, id := guardedDeleteFixture(t)
	volumes.onDelete = func(name, uid string) error {
		if !guard.records[deleteTestKey(id)].WriteStarted || uid != id.TargetUID {
			t.Fatal("external delete was not durably marked with frozen UID")
		}
		delete(volumes.records, name)
		return nil
	}
	result, err := u.DeleteVolume(context.Background(), id.TenantID, id.PVCName, id.TargetUID)
	if err != nil || result.State != DeletionGone || guard.completeCalls != 1 || guard.recordCalls != 0 || guard.records[deleteTestKey(id)].Phase != "completed" {
		t.Fatal("terminal evidence released token before completion", result, err)
	}
	if _, err := u.DeleteVolume(context.Background(), id.TenantID, id.PVCName, id.TargetUID); err != nil || len(volumes.deleteCalls) != 1 {
		t.Fatal("completed ticket resent delete", err)
	}
}

func TestGuardedDeleteRecoveryReservedRepeatsUsageAndCancelsOnlyUnsent(t *testing.T) {
	u, volumes, guard, id := guardedDeleteFixture(t)
	_, _, _ = guard.ReserveDelete(context.Background(), id, u.cfg.DeleteExecutor, deleteLeaseTTL)
	releaseTestDeleteLease(guard, id)
	volumes.pods = []Consumer{{Kind: "Pod", Namespace: testNS, Name: "external-consumer", Claim: id.PVCName}}
	worked, err := u.StepDeleteRecovery(context.Background())
	if !worked || !errors.Is(err, ErrVolumeInUse) || guard.cancelCalls != 1 || guard.markCalls != 0 || len(volumes.deleteCalls) != 0 {
		t.Fatal("reserved recovery skipped external usage", worked, err)
	}
	volumes.pods = nil
	if _, err := u.DeleteVolume(context.Background(), id.TenantID, id.PVCName, id.TargetUID); err != nil || guard.records[deleteTestKey(id)].Token.GuardEpoch != 2 {
		t.Fatal("cancelled ticket did not permit a new reservation", err)
	}
}

func TestGuardedDeleteMarkerFailureCannotSend(t *testing.T) {
	u, volumes, guard, id := guardedDeleteFixture(t)
	guard.markErr = ErrDeleteLease
	if _, err := u.DeleteVolume(context.Background(), id.TenantID, id.PVCName, id.TargetUID); !errors.Is(err, ErrBackendUnavailable) || len(volumes.deleteCalls) != 0 {
		t.Fatal("stale marker holder sent external write", err)
	}
}

func TestGuardedDeleteUnknownResponseKeepsBarrierAndRecoversOriginalUID(t *testing.T) {
	u, volumes, guard, id := guardedDeleteFixture(t)
	volumes.onDelete = func(string, string) error { return context.DeadlineExceeded }
	if _, err := u.DeleteVolume(context.Background(), id.TenantID, id.PVCName, id.TargetUID); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatal("unknown write returned accepted success", err)
	}
	record := guard.records[deleteTestKey(id)]
	if !record.WriteStarted || record.Phase != "observing" || record.LastWriteOutcome != "unknown" || guard.cancelCalls != 0 {
		t.Fatal("unknown result released protection", record)
	}
	// After a possible write, an external consumer cannot cancel the barrier.
	volumes.pods = []Consumer{{Kind: "Pod", Claim: id.PVCName}}
	volumes.onDelete = func(name, uid string) error {
		if uid != id.TargetUID {
			t.Fatal("recovery changed frozen deletion UID")
		}
		delete(volumes.records, name)
		return nil
	}
	worked, err := u.StepDeleteRecovery(context.Background())
	if !worked || err != nil || len(volumes.deleteCalls) != 2 || guard.cancelCalls != 0 || guard.records[deleteTestKey(id)].Phase != "completed" {
		t.Fatal("possible write was not safely recovered", worked, err)
	}
}

func TestGuardedDeleteActiveLeaseRetryCannotStealWriter(t *testing.T) {
	u, volumes, guard, id := guardedDeleteFixture(t)
	record, _, _ := guard.ReserveDelete(context.Background(), id, "other-writer", deleteLeaseTTL)
	if _, err := u.DeleteVolume(context.Background(), id.TenantID, id.PVCName, id.TargetUID); !errors.Is(err, ErrBackendUnavailable) || len(volumes.deleteCalls) != 0 || guard.records[deleteTestKey(id)].Token != record.Token {
		t.Fatal("active lease replay invented submission or stole writer", err)
	}
	record.WriteStarted, record.LastWriteOutcome = true, "submitted"
	guard.records[deleteTestKey(id)] = record
	if result, err := u.DeleteVolume(context.Background(), id.TenantID, id.PVCName, id.TargetUID); err != nil || result.State != DeletionSubmitted || len(volumes.deleteCalls) != 0 {
		t.Fatal("known submission was not replayable", result, err)
	}
}

func TestGuardedDeleteRecoveryAfterMarkerBeforeSending(t *testing.T) {
	u, volumes, guard, id := guardedDeleteFixture(t)
	record, _, err := guard.ReserveDelete(context.Background(), id, u.cfg.DeleteExecutor, deleteLeaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.MarkDeleteWriteStarted(context.Background(), id, record.Token); err != nil {
		t.Fatal(err)
	}
	// The process exits here before the external call. Expiration permits
	// takeover, while the possible-write marker and name barrier remain sticky.
	releaseTestDeleteLease(guard, id)
	worked, err := u.StepDeleteRecovery(context.Background())
	if !worked || err != nil || len(volumes.deleteCalls) != 1 || volumes.deleteCalls[0] != id.TargetUID || guard.cancelCalls != 0 || guard.records[deleteTestKey(id)].Phase != "completed" {
		t.Fatal("marked but unsent delete did not resume with original UID", worked, err)
	}
}

func TestGuardedDeleteRecoveryCompletesOldTicketWithoutDeletingNewUID(t *testing.T) {
	u, volumes, guard, id := guardedDeleteFixture(t)
	_, _, _ = guard.ReserveDelete(context.Background(), id, u.cfg.DeleteExecutor, deleteLeaseTTL)
	record := guard.records[deleteTestKey(id)]
	record.WriteStarted = true
	guard.records[deleteTestKey(id)] = record
	releaseTestDeleteLease(guard, id)
	volumes.records[id.PVCName].UID = "same-name-new-uid"
	worked, err := u.StepDeleteRecovery(context.Background())
	if !worked || !errors.Is(err, ErrIdentityChanged) || guard.completeCalls != 1 || guard.recordCalls != 0 || len(volumes.deleteCalls) != 0 {
		t.Fatal("replacement was deleted or terminal token lost", worked, err)
	}
	if record := guard.records[deleteTestKey(id)]; record.Identity.TargetUID != id.TargetUID || record.ObservedUID != "same-name-new-uid" || record.Phase != "completed" {
		t.Fatal("completion overwrote original ticket identity", record)
	}
	if _, err := u.DeleteVolume(context.Background(), id.TenantID, id.PVCName, id.TargetUID); !errors.Is(err, ErrIdentityChanged) {
		t.Fatal("public old-UID call reported replacement as its result", err)
	}
}
