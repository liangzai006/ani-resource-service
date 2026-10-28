package biz

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type switchObservationRepository struct {
	work           VirtualSwitchWork
	finishedState  string
	finishedReason Reason
	saves          int
}

func (r *switchObservationRepository) ClaimVirtualSwitch(context.Context, string, time.Duration) (VirtualSwitchWork, bool, error) {
	w := r.work
	w.Switch.Nodes = append([]VirtualSwitchNode(nil), w.Switch.Nodes...)
	return w, true, nil
}
func (r *switchObservationRepository) SaveVirtualSwitchNode(_ context.Context, _ VirtualSwitchWork, index int, node VirtualSwitchNode) error {
	r.work.Switch.Nodes[index] = node
	r.saves++
	return nil
}
func (r *switchObservationRepository) FinishVirtualSwitch(_ context.Context, _ VirtualSwitchWork, state string, reason Reason, _ time.Duration) error {
	r.finishedState, r.finishedReason = state, reason
	r.work.Switch.BindingState, r.work.Switch.Reason = state, reason
	r.work.Switch.UpdatedAt = r.work.Now
	if state == "degraded" {
		r.work.Switch.State = Degraded
	}
	return nil
}

type switchObservationProvider struct {
	inspect func(VirtualSwitchNode) (VirtualSwitchNode, error)
	patches int
}

func (p *switchObservationProvider) InspectVirtualSwitchNode(_ context.Context, _ VirtualSwitch, node VirtualSwitchNode) (VirtualSwitchNode, error) {
	return p.inspect(node)
}
func (p *switchObservationProvider) PatchVirtualSwitchNode(_ context.Context, _ VirtualSwitch, node VirtualSwitchNode, _ bool) (VirtualSwitchNode, error) {
	p.patches++
	node.Applied = true
	return node, nil
}

func switchObservationNode(t *testing.T, name string, at time.Time) VirtualSwitchNode {
	t.Helper()
	// Exercise the persisted JSON boundary as well as the in-memory worker.
	body, err := json.Marshal(map[string]any{"NodeName": name, "NodeUID": "uid-" + name, "Applied": true, "LastObservedAt": at})
	if err != nil {
		t.Fatal(err)
	}
	var node VirtualSwitchNode
	if err = json.Unmarshal(body, &node); err != nil {
		t.Fatal(err)
	}
	return node
}

func switchNodeObservationTime(t *testing.T, node VirtualSwitchNode) time.Time {
	t.Helper()
	body, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	var observed struct{ LastObservedAt time.Time }
	if err = json.Unmarshal(body, &observed); err != nil {
		t.Fatal(err)
	}
	return observed.LastObservedAt
}

func runSwitchObservation(t *testing.T, r *switchObservationRepository, p *switchObservationProvider) {
	t.Helper()
	w, err := NewVirtualSwitchWorker(r, p, uuid.NewString(), DefaultWorkerPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := w.Step(context.Background()); !worked || err != nil {
		t.Fatal("switch observation failed", worked, err)
	}
}

func TestVirtualSwitchTemporaryObservationGraceDoesNotRenewOnFailure(t *testing.T) {
	observed := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	r := &switchObservationRepository{work: VirtualSwitchWork{Switch: VirtualSwitch{EgressMetadata: EgressMetadata{State: Available}, BindingState: "configured", Nodes: []VirtualSwitchNode{switchObservationNode(t, "node-a", observed), switchObservationNode(t, "node-b", observed)}}}}
	p := &switchObservationProvider{inspect: func(node VirtualSwitchNode) (VirtualSwitchNode, error) {
		return node, &ProviderError{Kind: ProviderTemporary, Cause: context.DeadlineExceeded}
	}}
	for _, elapsed := range []time.Duration{10 * time.Second, 59 * time.Second, 61 * time.Second} {
		r.work.Now = observed.Add(elapsed)
		// A new worker demonstrates that grace is anchored in durable evidence,
		// rather than a process-local timer or the last failed attempt.
		runSwitchObservation(t, r, p)
		want := "configured"
		if elapsed > DefaultWorkerPolicy().StaleAfter {
			want = "degraded"
		}
		if r.finishedState != want || r.finishedReason != ProviderUnavailable {
			t.Fatalf("after %s: state=%q reason=%q, want %q and provider unavailable", elapsed, r.finishedState, r.finishedReason, want)
		}
	}
	if p.patches != 0 || r.saves != 0 {
		t.Fatal("failed reads changed provider state or renewed node evidence", p.patches, r.saves)
	}
}

func TestVirtualSwitchObservationGraceRequiresCompleteHealthyEvidence(t *testing.T) {
	observed := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name             string
		state            ResourceState
		bindingState     string
		otherObservation time.Time
		kind             ProviderFailure
		want             string
	}{
		{"missing_node_evidence", Available, "configured", time.Time{}, ProviderTemporary, "degraded"},
		{"stale_node_evidence", Available, "configured", observed.Add(-time.Minute), ProviderTemporary, "degraded"},
		{"future_node_evidence", Available, "configured", observed.Add(time.Hour), ProviderTemporary, "degraded"},
		{"not_configured", Available, "pending", observed, ProviderTemporary, "degraded"},
		{"already_degraded", Degraded, "degraded", observed, ProviderTemporary, "degraded"},
		{"ownership_conflict", Available, "configured", observed, ProviderConflict, "degraded"},
		{"deletion", Deleting, "cleaning", observed, ProviderTemporary, "cleaning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &switchObservationRepository{work: VirtualSwitchWork{Now: observed.Add(10 * time.Second), Switch: VirtualSwitch{EgressMetadata: EgressMetadata{State: tc.state}, BindingState: tc.bindingState, Nodes: []VirtualSwitchNode{switchObservationNode(t, "node-a", observed), switchObservationNode(t, "node-b", tc.otherObservation)}}}}
			p := &switchObservationProvider{inspect: func(node VirtualSwitchNode) (VirtualSwitchNode, error) { return node, &ProviderError{Kind: tc.kind} }}
			runSwitchObservation(t, r, p)
			if r.finishedState != tc.want {
				t.Fatalf("state=%q, want %q", r.finishedState, tc.want)
			}
		})
	}
}

func TestVirtualSwitchSuccessfulInspectionRenewsOnlyObservedNodeEvidence(t *testing.T) {
	observed := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	now := observed.Add(10 * time.Second)
	providerObserved := observed.Add(5 * time.Second)
	r := &switchObservationRepository{work: VirtualSwitchWork{Now: now, Switch: VirtualSwitch{EgressMetadata: EgressMetadata{State: Available}, BindingState: "configured", Nodes: []VirtualSwitchNode{switchObservationNode(t, "node-a", observed), switchObservationNode(t, "node-b", observed)}}}}
	p := &switchObservationProvider{inspect: func(node VirtualSwitchNode) (VirtualSwitchNode, error) {
		if node.NodeName == "node-b" {
			return node, &ProviderError{Kind: ProviderTemporary, Cause: errors.New("node-b observation timed out")}
		}
		return switchObservationNode(t, node.NodeName, providerObserved), nil
	}}
	runSwitchObservation(t, r, p)
	if got := switchNodeObservationTime(t, r.work.Switch.Nodes[0]); !got.Equal(providerObserved) {
		t.Fatalf("successful node evidence=%v, want provider evidence %v rather than claim time %v", got, providerObserved, now)
	}
	if got := switchNodeObservationTime(t, r.work.Switch.Nodes[1]); !got.Equal(observed) {
		t.Fatalf("failed node evidence was renewed: %v", got)
	}
	if r.finishedState != "configured" || p.patches != 0 {
		t.Fatal("fresh previously healthy switch was degraded or mutated", r.finishedState, p.patches)
	}
}

func TestVirtualSwitchWorkerRejectsUnboundedObservationFreshness(t *testing.T) {
	r := &switchObservationRepository{}
	p := &switchObservationProvider{}
	policy := DefaultWorkerPolicy()
	policy.StaleAfter = policy.ObserveEvery
	if _, err := NewVirtualSwitchWorker(r, p, uuid.NewString(), policy); err == nil {
		t.Fatal("accepted policy without a usable stale observation bound")
	}
}

func TestVirtualSwitchObserverRetainsFailedNodeAndDoesNotClaimFreshObservation(t *testing.T) {
	observed := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	r := &switchObservationRepository{work: VirtualSwitchWork{Now: observed.Add(10 * time.Second), Switch: VirtualSwitch{EgressMetadata: EgressMetadata{State: Available}, BindingState: "configured", Nodes: []VirtualSwitchNode{switchObservationNode(t, "node-a", observed)}}}}
	cause := errors.New("node-a observation timed out")
	p := &switchObservationProvider{inspect: func(node VirtualSwitchNode) (VirtualSwitchNode, error) {
		return node, &ProviderError{Kind: ProviderTemporary, Cause: cause}
	}}
	calls := 0
	w, err := NewVirtualSwitchWorker(r, p, uuid.NewString(), DefaultWorkerPolicy(), func(_ context.Context, _ VirtualSwitchWork, progress VirtualSwitchProgress, finishErr error) {
		calls++
		if finishErr != nil || !errors.Is(progress.ProviderError, cause) || progress.Reason != ProviderUnavailable || progress.BindingState != "configured" || progress.Observed || !progress.CollectedAt.IsZero() || progress.NodeName != "node-a" || progress.NodeUID != "uid-node-a" || progress.Action != "inspect" {
			t.Errorf("missing failed node diagnostics or invented observation: %+v, err=%v", progress, finishErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Step(context.Background()); err != nil || calls != 1 {
		t.Fatal("observer missing", calls, err)
	}
}

func TestVirtualSwitchEmptyObservationPreservesExistingNodeFingerprintJSON(t *testing.T) {
	body, err := json.Marshal(VirtualSwitchNode{NodeName: "node-a", NodeUID: "uid-a"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["LastObservedAt"]; present {
		t.Fatal("empty observation changed legacy membership fingerprint input", string(body))
	}
}
