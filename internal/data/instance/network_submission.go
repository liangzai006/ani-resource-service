package instance

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
)

func initialNetworkSubmission(i biz.Instance) *biz.NetworkRuntime {
	if i.Spec.Network == nil {
		return nil
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("instance-network-v2\n"+i.TenantID+"\n"+i.ID+"\n"+i.CreateOperationID)).String()
	n := &biz.NetworkRuntime{SubmissionID: id, RequestKey: "instance-primary:" + id, Generation: 1, SnapshotRevision: 1, State: "open", DesiredRunState: i.RuntimeDesired, NetworkState: "pending"}
	if i.Desired == biz.Deleted {
		n.State = "closed"
		n.FinalizationID = networkFinalization(i)
		closed := i.CreatedAt
		if closed.IsZero() {
			closed = i.UpdatedAt
		}
		n.ClosedAt = &closed
		n.Cleaned = true
		n.NetworkState = "not_requested"
	}
	return n
}
func networkFinalization(i biz.Instance) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("instance-network-finalization-v2\n"+i.TenantID+"\n"+i.ID+"\n"+i.CreateOperationID)).String()
}
func networkIntent(i *biz.Instance) {
	if i.Network == nil {
		return
	}
	n := i.Network
	changed := n.DesiredRunState != i.RuntimeDesired
	n.DesiredRunState = i.RuntimeDesired
	if i.Desired == biz.Deleted && n.State == "open" {
		n.State = "closing"
		n.FinalizationID = networkFinalization(*i)
		changed = true
	}
	if changed {
		n.SnapshotRevision++
		// A new durable intent invalidates the prior Running qualification.
		if i.Observed == biz.Running {
			i.Observed = "pending"
			i.Reason = "WAITING_FOR_NETWORK"
		}
	}
}
func saveNetworkSubmission(ctx context.Context, tx pgx.Tx, i biz.Instance) error {
	if i.Network == nil {
		return nil
	}
	n := i.Network
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	var finalization *string
	if n.FinalizationID != "" {
		finalization = &n.FinalizationID
	}
	tag, err := tx.Exec(ctx, `INSERT INTO instance.network_submissions(tenant_id,instance_id,submission_id,request_key,generation,snapshot_revision,state,attachment_id,finalization_id,snapshot) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(tenant_id,instance_id) DO UPDATE SET snapshot_revision=EXCLUDED.snapshot_revision,state=EXCLUDED.state,attachment_id=EXCLUDED.attachment_id,finalization_id=EXCLUDED.finalization_id,snapshot=EXCLUDED.snapshot,updated_at=clock_timestamp() WHERE network_submissions.submission_id=EXCLUDED.submission_id AND network_submissions.request_key=EXCLUDED.request_key AND network_submissions.generation=EXCLUDED.generation AND network_submissions.snapshot_revision<=EXCLUDED.snapshot_revision`, i.TenantID, i.ID, n.SubmissionID, n.RequestKey, n.Generation, n.SnapshotRevision, n.State, n.AttachmentID, finalization, b)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return biz.ErrVersion
	}
	return nil
}
func mergeNetworkResult(current *biz.NetworkRuntime, result *biz.NetworkRuntime) {
	if current == nil || result == nil || current.SubmissionID != result.SubmissionID || current.Generation != result.Generation {
		return
	}
	current.PrepareStarted = current.PrepareStarted || result.PrepareStarted
	if current.FinalizationID != "" && current.FinalizationID == result.FinalizationID {
		current.ReleaseStarted = current.ReleaseStarted || result.ReleaseStarted
		current.ReleaseAcknowledged = current.ReleaseAcknowledged || result.ReleaseAcknowledged
	}
	if current.AttachmentID == "" && result.AttachmentID != "" {
		current.AttachmentID = result.AttachmentID
		current.BindingRevision = result.BindingRevision
		current.PrimaryNetworkRef = result.PrimaryNetworkRef
	}
	if current.AttachmentID == result.AttachmentID && result.AttachmentVersion > current.AttachmentVersion {
		current.AttachmentVersion = result.AttachmentVersion
	}
}
