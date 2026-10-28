package biz

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type VirtualSwitchWorker struct {
	repository VirtualSwitchWorkRepository
	provider   VirtualSwitchGroupProvider
	owner      string
	policy     WorkerPolicy
	observer   func(context.Context, VirtualSwitchWork, VirtualSwitchProgress, error)
}

// VirtualSwitchProgress describes one attempt without treating retained healthy
// state during a temporary read failure as a successful observation.
type VirtualSwitchProgress struct {
	ProviderError error `json:"-"`
	BindingState  string
	Reason        Reason
	Action        string
	NodeName      string
	NodeUID       string
	Observed      bool
	CollectedAt   time.Time
}

func NewVirtualSwitchWorker(r VirtualSwitchWorkRepository, p VirtualSwitchGroupProvider, owner string, policy WorkerPolicy, observers ...func(context.Context, VirtualSwitchWork, VirtualSwitchProgress, error)) (*VirtualSwitchWorker, error) {
	id, err := uuid.Parse(owner)
	if len(observers) > 1 || r == nil || p == nil || err != nil || id == uuid.Nil || policy.RequestTimeout <= 0 || policy.Lease < 3*policy.RequestTimeout || policy.ObserveEvery <= 0 || policy.StaleAfter <= policy.ObserveEvery {
		return nil, Fail(InvalidArgument, "invalid switch worker dependencies or policy")
	}
	w := &VirtualSwitchWorker{repository: r, provider: p, owner: id.String(), policy: policy}
	if len(observers) == 1 {
		w.observer = observers[0]
	}
	return w, nil
}

func (w *VirtualSwitchWorker) Step(ctx context.Context) (worked bool, err error) {
	defer func() {
		if errors.Is(err, ErrLeaseLost) {
			err = nil
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, w.policy.Lease)
	defer cancel()
	work, found, err := w.repository.ClaimVirtualSwitch(ctx, w.owner, w.policy.Lease)
	if err != nil || !found {
		return found, err
	}
	sw := work.Switch
	sw.Nodes = append([]VirtualSwitchNode(nil), sw.Nodes...)
	deleting := sw.State == Deleting
	progress := VirtualSwitchProgress{BindingState: sw.BindingState, Reason: sw.Reason}
	defer func() {
		if w.observer != nil {
			w.observer(ctx, work, progress, err)
		}
	}()
	finish := func(state string, reason Reason, delay time.Duration) (bool, error) {
		progress.BindingState, progress.Reason = state, reason
		return true, w.repository.FinishVirtualSwitch(ctx, work, state, reason, delay)
	}
	for index, node := range sw.Nodes {
		progress.Action, progress.NodeName, progress.NodeUID = "inspect", node.NodeName, node.NodeUID
		call, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
		current, inspectErr := w.provider.InspectVirtualSwitchNode(call, sw, node)
		cancel()
		if inspectErr != nil {
			progress.ProviderError = inspectErr
			state := "degraded"
			if deleting {
				state = "cleaning"
			} else if providerKind(inspectErr) == ProviderTemporary && sw.State == Available && sw.BindingState == "configured" && freshVirtualSwitchObservation(work.Switch, work.Now, w.policy.StaleAfter) {
				state = "configured"
			}
			return finish(state, providerReason(inspectErr), w.policy.ObserveEvery)
		}
		if (!deleting && current.Applied) || (deleting && current.State == "cleaned") {
			if err = w.repository.SaveVirtualSwitchNode(ctx, work, index, current); err != nil {
				return true, err
			}
			sw.Nodes[index] = current
			continue
		}
		// The exact precondition and intended owner are durable before dispatch.
		current.Prepared = true
		current.Unknown = true
		if err = w.repository.SaveVirtualSwitchNode(ctx, work, index, current); err != nil {
			return true, err
		}
		call, cancel = context.WithTimeout(ctx, w.policy.RequestTimeout)
		progress.Action = "patch"
		applied, patchErr := w.provider.PatchVirtualSwitchNode(call, sw, current, deleting)
		cancel()
		if patchErr != nil {
			progress.ProviderError = patchErr
			state := "degraded"
			if deleting {
				state = "cleaning"
			}
			return finish(state, providerReason(patchErr), w.policy.ObserveEvery)
		}
		if err = w.repository.SaveVirtualSwitchNode(ctx, work, index, applied); err != nil {
			return true, err
		}
		state := "pending"
		if deleting {
			state = "cleaning"
		}
		return finish(state, "", w.policy.RetryMin)
	}
	if deleting {
		return finish("cleaned", "", w.policy.ObserveEvery)
	}
	progress.Observed = true
	progress.CollectedAt = virtualSwitchObservationTime(sw)
	progress.Action, progress.NodeName, progress.NodeUID = "", "", ""
	return finish("configured", "", w.policy.ObserveEvery)
}

func virtualSwitchObservationTime(sw VirtualSwitch) time.Time {
	var oldest time.Time
	for _, node := range sw.Nodes {
		if !node.Applied || node.Unknown || node.LastObservedAt.IsZero() {
			return time.Time{}
		}
		if oldest.IsZero() || node.LastObservedAt.Before(oldest) {
			oldest = node.LastObservedAt
		}
	}
	return oldest
}

func freshVirtualSwitchObservation(sw VirtualSwitch, now time.Time, staleAfter time.Duration) bool {
	oldest := virtualSwitchObservationTime(sw)
	if oldest.IsZero() || now.Before(oldest) || now.Sub(oldest) > staleAfter {
		return false
	}
	for _, node := range sw.Nodes {
		if node.LastObservedAt.After(now) {
			return false
		}
	}
	return true
}
