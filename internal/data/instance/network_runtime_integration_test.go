//go:build instanceintegration

package instance_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/instance"
)

func networkCreate() biz.Create {
	c := cpuCreate()
	c.Spec.ContractVersion = 2
	c.Spec.WorkspaceMountPath = "/workspace"
	c.Spec.Network = &biz.NetworkSelection{VPCID: "vpc_" + strings.Repeat("a", 32), SubnetID: "subnet_" + strings.Repeat("b", 32)}
	return c
}
func TestInstanceNetworkSubmissionAdmissionAndIntentAreAtomic(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := networkCreate()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	i, err := f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if i.Network == nil || i.Network.State != "open" || i.Network.SnapshotRevision != 1 || i.Network.DesiredRunState != biz.Running || !biz.UUID(i.Network.SubmissionID) || i.Network.RequestKey == "" {
		t.Fatal("admission did not persist stable network submission", i.Network)
	}
	frozen, _ := json.Marshal(i.Spec)
	submission, key := i.Network.SubmissionID, i.Network.RequestKey
	if _, err = f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	command(t, f, c, i, "stop")
	i, err = f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if i.Network.State != "open" || i.Network.DesiredRunState != biz.Stopped || i.Network.SnapshotRevision != 2 || i.Network.SubmissionID != submission || i.Network.RequestKey != key {
		t.Fatal("Stop closed/replaced logical submission", i.Network)
	}
	command(t, f, c, i, "delete")
	i, err = f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if i.Network.State != "closing" || i.Network.DesiredRunState != biz.Deleted || i.Network.SnapshotRevision != 3 || !biz.UUID(i.Network.FinalizationID) {
		t.Fatal("Delete omitted persistent closing fence", i.Network)
	}
	current, _ := json.Marshal(i.Spec)
	if string(frozen) != string(current) {
		t.Fatal("dynamic network observation changed frozen create spec")
	}
	var revision int64
	var state string
	if err = f.runtime.QueryRow(ctx, `SELECT snapshot_revision,state FROM instance.network_submissions WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID).Scan(&revision, &state); err != nil || revision != 3 || state != "closing" {
		t.Fatal("snapshot and narrow submission table diverged", revision, state, err)
	}
	if _, err = f.runtime.Exec(ctx, `INSERT INTO instance.network_submissions(tenant_id,instance_id,submission_id,request_key,generation,snapshot_revision,state,snapshot) VALUES($1,$2,$3,'forged',1,1,'open','{}')`, uuid.NewString(), c.ID, uuid.NewString()); err == nil {
		t.Fatal("cross-tenant submission FK accepted")
	}
}
func TestInstanceNetworkClosingWaitsForUnknownNotebookCreate(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := networkCreate()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	work, err := f.repo.Claim(ctx, "unknown-create", time.Minute)
	if err != nil || work == nil {
		t.Fatal(err)
	}
	m := biz.Mutation{Kind: "create_notebook", OperationID: c.OperationID, Version: work.Instance.Version}
	if err = f.repo.BeginMutation(ctx, *work, m); err != nil {
		t.Fatal(err)
	}
	i, err := f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	command(t, f, c, i, "delete")
	i, err = f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changed := biz.ReconcileSubmission(&i, biz.Observation{}); changed || i.Network.State != "closing" || i.Network.ClosedAt != nil {
		t.Fatal("one absence closed an unknown CREATE", i.Network)
	}
	if _, err = f.repo.Get(ctx, uuid.NewString(), c.ID); !errors.Is(err, biz.ErrNotFound) {
		t.Fatal("foreign submission visible", err)
	}
}

func TestInstanceNetworkOutageBlocksOriginalGPURefundUntilCleanup(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := create()
	c.Spec.ContractVersion = 2
	c.Spec.WorkspaceMountPath = "/workspace"
	c.Spec.Network = &biz.NetworkSelection{VPCID: "vpc_" + strings.Repeat("a", 32), SubnetID: "subnet_" + strings.Repeat("b", 32)}
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	endpoint := &releaseEndpoint{}
	reporter, err := data.NewQuota(f.repo, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := biz.NewWorker(f.repo, &provider{}, "gpu-network-outage")
	if err != nil {
		t.Fatal(err)
	}
	// No Network port represents an unavailable in-process dependency. No
	// network completion is substituted: the final attachment stays pending.
	i := step(t, f, worker, c)
	command(t, f, c, i, "delete")
	for round := 0; round < 12; round++ {
		i = step(t, f, worker, c)
	}
	if !i.ComputeCleaned || !i.WorkspaceCleaned || i.QuotaReturned || i.Network.Cleaned || i.Observed == biz.Deleted || !i.CleanupCompletedAt.IsZero() {
		t.Fatal("network outage fabricated complete owner cleanup", i.Network, i.Reason)
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || dispatched || endpoint.calls != 0 {
		t.Fatal("network outage permitted early original refund", dispatched, err, endpoint.calls)
	}
	var events int
	if err = f.runtime.QueryRow(ctx, `SELECT count(*) FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID).Scan(&events); err != nil || events != 0 {
		t.Fatal("unfinished network cleanup created a refund event", events, err)
	}
}

// The frozen command must survive shutting the new-write gates. These external
// image and Kubernetes boundaries deliberately reject fresh resolution; intake
// uses the already frozen command and real tenant-scoped PostgreSQL transaction.
type frozenNetworkIntakeBoundary struct{}

func (frozenNetworkIntakeBoundary) Resolve(context.Context, string, string, string) (biz.ResolvedImage, error) {
	return biz.ResolvedImage{}, errors.New("unexpected image re-resolution")
}
func (frozenNetworkIntakeBoundary) ValidateEnvironment(context.Context, string, string, string, string) error {
	return errors.New("unexpected environment re-resolution")
}
func TestInstanceNetworkSubmissionTrustedServiceAcceptsFrozenSelectionAfterWritesClose(t *testing.T) {
	f := database(t)
	c := networkCreate()
	s := c.Spec
	intake, err := service.NewService(f.repo, computeResolver(t, f, s.ClusterID), frozenNetworkIntakeBoundary{}, frozenNetworkIntakeBoundary{}, service.Config{NamespacePrefix: "tenant-", ClusterID: s.ClusterID, ServiceAccount: s.ServiceAccount, StorageClass: s.StorageClass, WorkspaceSize: s.WorkspaceSize, Shell: s.Shell, UID: s.UID, GID: s.GID})
	if err != nil {
		t.Fatal(err)
	}
	request := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: c.TenantID, ResourceId: c.ID, OperationId: c.OperationID}, Spec: &pb.CreateSpec{Name: s.Name, ImageId: s.ImageID, ImageScope: s.ImageScope, PresetId: s.PresetID, ImageReference: s.ImageReference, Cpu: s.CPU, Memory: s.Memory, ClusterId: s.ClusterID, Namespace: s.Namespace, ServiceAccount: s.ServiceAccount, StorageClass: s.StorageClass, WorkspaceSize: s.WorkspaceSize, Shell: s.Shell, Uid: s.UID, Gid: s.GID, InstanceKind: s.InstanceKind, ComputeType: s.ComputeType, Scheduling: &pb.SchedulingOptions{PodPlacement: pb.SchedulingOptions_NONE.Enum()}, InstanceContractVersion: 2, WorkspaceMountPath: s.WorkspaceMountPath, Network: &pb.NetworkSelection{VpcId: s.Network.VPCID, SubnetId: s.Network.SubnetID}}}
	request.Context.RequestDigest, err = pb.CommandDigest(request.Context, request.Spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: c.TenantID, Actor: "governance:user:42"})
	ack, err := intake.AcceptCreate(ctx, request)
	if err != nil || !ack.GetDurableOwnerAck().GetAccepted() {
		t.Fatal("frozen network command blocked after new-write gates closed", err)
	}
	original, err := f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if original.Spec.Network == nil || *original.Spec.Network != *s.Network || original.Network == nil || original.Network.State != "open" || original.Network.SnapshotRevision != 1 || !biz.UUID(original.Network.SubmissionID) {
		t.Fatal("trusted intake lost frozen selection or durable submission", original.Network)
	}
	if _, err = intake.AcceptCreate(ctx, request); err != nil {
		t.Fatal("frozen command replay failed", err)
	}
	replay, err := f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Network.SubmissionID != original.Network.SubmissionID || replay.Network.RequestKey != original.Network.RequestKey || replay.Network.SnapshotRevision != 1 {
		t.Fatal("replay replaced durable network submission", replay.Network)
	}
}
