package storage

import (
	"context"
	"errors"
	"time"
)

var ErrDeleteLease = errors.New("STORAGE_DELETE_LEASE_LOST")

// PVCIdentity is frozen when the Instance owner creates a name guard ticket.
// The guard's physical key excludes UID; claimName is the workload reference.
type PVCIdentity struct {
	TenantID, ClusterID, Namespace, PVCName, TargetUID string
}

type DeleteToken struct {
	TicketID   string
	GuardEpoch int64
	LeaseOwner string
	LeaseEpoch int64
	LeaseUntil time.Time
}

type DeleteRecord struct {
	Identity                                      PVCIdentity
	Token                                         DeleteToken
	Phase                                         string
	WriteStarted                                  bool
	LastWriteOutcome, LastObservation, LastReason string
	ObservedUID                                   string
	RetryAt, CreatedAt, UpdatedAt                 time.Time
}

type DeleteObservation struct {
	// State: present, terminating, absent, identity_changed, unavailable.
	State, ObservedUID, WriteOutcome, Reason string
	RetryAt                                  time.Time
}

// DeleteGuardStats is a bounded operational view, without PVC identities or
// credentials as metric labels. Counters cover this process lifetime.
type DeleteGuardStats struct {
	Incomplete                        int64
	OldestAgeSeconds                  float64
	ReferenceConflicts, UnknownWrites uint64
	FailureReasons                    map[string]int64
}

// BorrowedPVCGuard is implemented by the Instance owner. Storage never reads
// Instance's private tables. All mutations compare the full identity and token.
type BorrowedPVCGuard interface {
	// ReserveDelete returns mayAdvance only when this call acquired a lease.
	// An active lease replay must not issue a second writer's token. The
	// maintenance gate applies to new tickets after the replay lookup.
	ReserveDelete(context.Context, PVCIdentity, string, time.Duration) (DeleteRecord, bool, error)
	ClaimDeleteRecovery(context.Context, string, time.Duration) (*DeleteRecord, error)
	MarkDeleteWriteStarted(context.Context, PVCIdentity, DeleteToken) error
	// Record releases this turn's lease. Terminal proof calls Complete directly.
	RecordDeleteObservation(context.Context, PVCIdentity, DeleteToken, DeleteObservation) error
	CancelUnsentDelete(context.Context, PVCIdentity, DeleteToken, string) error
	CompleteDelete(context.Context, PVCIdentity, DeleteToken, DeleteObservation) error
}
