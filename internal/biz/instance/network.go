package instance

import (
	"context"
	"reflect"
	"time"
)

// Network owns Attachment transactions; Instance only publishes its durable
// workload submission and consumes these narrow lifecycle operations.
type WorkloadReference struct{ APIVersion, Kind, Namespace, Name, UID string }
type ControllerReference struct{ Reference, Owner WorkloadReference }
type PodReference struct{ Reference, Owner WorkloadReference }
type NetworkRuntime struct {
	SubmissionID, RequestKey                         string
	Generation, SnapshotRevision                     int64
	State, DesiredRunState, FinalizationID           string
	ClosedAt                                         *time.Time
	RootController                                   WorkloadReference
	Controllers                                      []ControllerReference
	CurrentPods, RetiredPods                         []PodReference
	AttachmentID, BindingRevision, PrimaryNetworkRef string
	AttachmentVersion, ConfirmedSnapshotRevision     int64
	NetworkState, Reason                             string
	PrepareStarted                                   bool
	ReleaseStarted, ReleaseAcknowledged              bool
	Cleaned                                          bool
}
type NetworkBinding struct {
	AttachmentID, BindingRevision, PrimaryNetworkRef string
	Version                                          int64
}
type NetworkObservation struct {
	State, Reason, PodUID     string
	Version, SnapshotRevision int64
}
type NetworkPort interface {
	ValidateSelection(context.Context, string, string, string, NetworkSelection) error
	Prepare(context.Context, Instance) (NetworkBinding, error)
	Observe(context.Context, Instance) (NetworkObservation, error)
	Confirm(context.Context, Instance) error
	Release(context.Context, Instance) error
}
type InstanceReader interface {
	Get(context.Context, string, string) (Instance, error)
}

func CloneNetwork(n *NetworkRuntime) *NetworkRuntime {
	if n == nil {
		return nil
	}
	out := *n
	out.Controllers = append([]ControllerReference(nil), n.Controllers...)
	out.CurrentPods = append([]PodReference(nil), n.CurrentPods...)
	out.RetiredPods = append([]PodReference(nil), n.RetiredPods...)
	return &out
}

// ReconcileSubmission is called only after a successful provider observation.
// A disappeared Pod retires its identity; old generations never become current.
func ReconcileSubmission(i *Instance, o Observation) bool {
	if i.Network == nil {
		return false
	}
	old := CloneNetwork(i.Network)
	n := i.Network
	n.DesiredRunState = i.RuntimeDesired
	if o.NotebookUID != "" && n.RootController.UID == "" {
		n.RootController = WorkloadReference{"kubeflow.org/v1", "Notebook", i.Spec.Namespace, i.Spec.NotebookName, o.NotebookUID}
	}
	if o.StatefulSetUID != "" {
		ref := ControllerReference{WorkloadReference{"apps/v1", "StatefulSet", i.Spec.Namespace, i.Spec.NotebookName, o.StatefulSetUID}, n.RootController}
		exists := false
		for _, v := range n.Controllers {
			if v == ref {
				exists = true
			}
		}
		if !exists {
			n.Controllers = append(n.Controllers, ref)
		}
	}
	next := []PodReference(nil)
	if o.PodExists && o.PodUID != "" {
		for _, controller := range n.Controllers {
			if controller.Reference.UID == o.StatefulSetUID {
				next = []PodReference{{WorkloadReference{"v1", "Pod", i.Spec.Namespace, o.PodName, o.PodUID}, controller.Reference}}
				break
			}
		}
	}
	for _, pod := range n.CurrentPods {
		if len(next) > 0 && next[0] == pod {
			continue
		}
		exists := false
		for _, old := range n.RetiredPods {
			if old == pod {
				exists = true
			}
		}
		if !exists {
			n.RetiredPods = append(n.RetiredPods, pod)
		}
	}
	n.CurrentPods = next
	// Root and historic controller identities remain in a closed snapshot so
	// Network can independently prove their disappearance.
	if n.State == "closing" && !o.NotebookExists && !o.PodExists && o.StatefulSetUID == "" && i.Pending == nil {
		closed := time.Now().UTC()
		n.State = "closed"
		n.ClosedAt = &closed
	}
	changed := !reflect.DeepEqual(old, n)
	if changed {
		n.SnapshotRevision++
	}
	return changed
}
