package biz

import "time"

// ObservationProof identifies a real collection, not a cache access or commit.
// Hash is an opaque content digest. It is never ordered like a Kubernetes RV.
// CoveredGeneration belongs to this resource's durable notification counter.
type ObservationProof struct {
	CollectedAt time.Time
	// A complete API collection can include independently dated physical
	// facts. Use its start for the concurrency fence, retaining CollectedAt
	// as the weakest source time for resource freshness.
	CollectionStartedAt time.Time
	Hash                string
	CoveredGeneration   int64
}

type ObservationRequirement struct {
	RequestedGeneration int64
	AppliedAt           time.Time
	PreviousCollectedAt time.Time
	Hash                string
}

// Covers disallows a different fact collected before a previously committed
// fact. Equivalent content may be reused, but still needs notification coverage.
func (p ObservationProof) Covers(r ObservationRequirement) bool {
	fenceTime := p.CollectedAt
	if !p.CollectionStartedAt.IsZero() {
		fenceTime = p.CollectionStartedAt
		if !r.PreviousCollectedAt.IsZero() && p.CollectedAt.Before(r.PreviousCollectedAt) {
			return false
		}
	}
	return !p.CollectedAt.IsZero() && p.CoveredGeneration >= r.RequestedGeneration &&
		(!fenceTime.Before(r.AppliedAt) || (p.Hash != "" && p.Hash == r.Hash))
}
