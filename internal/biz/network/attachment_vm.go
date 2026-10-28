package biz

import (
	"context"
	"encoding/hex"
	"strings"
)

type VMNetworkExpectation struct {
	NADName, NADUID, NADConfigDigest string
}

type VMNetworkSelection struct {
	TenantID, VPCID, SubnetID, ClusterID, Namespace, BindingRevision, KCNSubnetRef string
}

type VMPrimaryPlan struct {
	FormatVersion                                             int32
	Namespace, NADName, NADUID, NADConfigDigest, KCNSubnetRef string
	TenantID, InstanceID, AttachmentID, SubmissionID          string
	Generation                                                int64
}

type VMCurrentAddress struct {
	NICName, Address, LauncherPodUID, VMIUID string
}

type VMNetworkResolver interface {
	ResolveVMNetwork(context.Context, VMNetworkSelection, VMNetworkExpectation) (VMPrimaryPlan, error)
}

// VMNetworkPreparer is an optional, ancillary part of Network's existing
// durable Subnet reconciliation. Its failure does not change ordinary network
// availability; VM admission requires its separately persisted ready identity.
type VMNetworkPreparer interface {
	PrepareVMNetwork(context.Context, ProviderTarget) error
}

// VMReader is implemented by the VM domain. Instance IDs in this mode are VM
// IDs; this port must never fall through to the existing Instance repository.
type VMReader interface {
	GetVMNetworkSubmission(context.Context, Attachment) (ConsumerSubmission, error)
}

type vmConsumerRouter struct {
	legacy InstanceConsumer
	vm     VMReader
}

func NewVMConsumerRouter(legacy InstanceConsumer, vm VMReader) InstanceConsumer {
	return &vmConsumerRouter{legacy: legacy, vm: vm}
}

func (r *vmConsumerRouter) GetSubmission(ctx context.Context, a Attachment) (ConsumerSubmission, error) {
	if a.WorkloadMode == "vm_managed" {
		if r.vm == nil {
			return ConsumerSubmission{}, Fail(DependencyUnavailable, "VM network reader unavailable")
		}
		return r.vm.GetVMNetworkSubmission(ctx, a)
	}
	if a.WorkloadMode != "" && a.WorkloadMode != "single_pod" && a.WorkloadMode != "controller_managed" {
		return ConsumerSubmission{}, Fail(InvalidArgument, "unknown consumer mode")
	}
	if r.legacy == nil {
		return ConsumerSubmission{}, Fail(DependencyUnavailable, "instance network reader unavailable")
	}
	return r.legacy.GetSubmission(ctx, a)
}

func ValidVMNetworkExpectation(e *VMNetworkExpectation) bool {
	if e == nil || !namespaceName.MatchString(e.NADName) || e.NADUID == "" || len(e.NADUID) > 128 || len(e.NADConfigDigest) != 64 || strings.ToLower(e.NADConfigDigest) != e.NADConfigDigest {
		return false
	}
	_, err := hex.DecodeString(e.NADConfigDigest)
	return err == nil
}

func validVMConsumer(a Attachment, c ConsumerSubmission) bool {
	if c.ProtocolVersion != 3 || c.SnapshotRevision < 1 || c.TenantID != a.TenantID || c.InstanceID != a.InstanceID || c.SubmissionID != a.SubmissionID || c.Generation != a.Generation || c.AttachmentID != a.ID || c.ClusterID != a.ClusterID || c.Namespace != a.Namespace || len(c.CurrentPods) > 1 || len(c.Controllers) > 4096 || len(c.RetiredPods) > 4096 {
		return false
	}
	if c.DesiredRunState != "running" && c.DesiredRunState != "stopped" && c.DesiredRunState != "deleted" {
		return false
	}
	if c.RootController.UID == "" {
		if c.RootController != (WorkloadReference{}) || len(c.Controllers) != 0 || len(c.CurrentPods) != 0 || len(c.RetiredPods) != 0 {
			return false
		}
	} else if !referenceValid(c.RootController, "kubevirt.io/v1", "VirtualMachine", a.Namespace) {
		return false
	}
	controllers := map[string]WorkloadReference{}
	for _, item := range c.Controllers {
		if !referenceValid(item.Reference, "kubevirt.io/v1", "VirtualMachineInstance", a.Namespace) || item.Owner != c.RootController || controllers[item.Reference.UID].UID != "" {
			return false
		}
		controllers[item.Reference.UID] = item.Reference
	}
	seen := map[string]bool{}
	for _, group := range [][]PodReference{c.CurrentPods, c.RetiredPods} {
		for _, pod := range group {
			if !referenceValid(pod.Reference, "v1", "Pod", a.Namespace) || controllers[pod.Owner.UID] != pod.Owner || seen[pod.Reference.UID] {
				return false
			}
			seen[pod.Reference.UID] = true
		}
	}
	if c.State == "open" {
		return c.FinalizationID == "" && c.ClosedAt == nil
	}
	if (c.State != "closing" && c.State != "closed") || c.DesiredRunState != "deleted" || !UUIDFinalization(c.FinalizationID) {
		return false
	}
	return c.State != "closed" || (c.ClosedAt != nil && !c.ClosedAt.IsZero() && len(c.CurrentPods) == 0)
}

// VMClosedProtocolRecoveryMatches authorizes only cleanup of an unchanged,
// already accepted closure. It grants neither a current Pod nor address use.
func VMClosedProtocolRecoveryMatches(a Attachment, c ConsumerSubmission) bool {
	if a.WorkloadMode != "vm_managed" || a.State != Releasing || a.ConsumerSnapshot == nil || a.SnapshotRevision < 1 || a.SnapshotHash == "" || a.RootController.UID == "" || a.FinalizationID == "" {
		return false
	}
	return c.State == "closed" && c.DesiredRunState == "deleted" && len(c.CurrentPods) == 0 && c.FinalizationID == a.FinalizationID && c.RootController == a.RootController &&
		CompareConsumerSnapshot(a, c) == SnapshotAccepted && c.SnapshotRevision == a.SnapshotRevision && ConsumerSnapshotHash(c) == a.SnapshotHash && ConsumerSnapshotHash(*a.ConsumerSnapshot) == a.SnapshotHash
}
