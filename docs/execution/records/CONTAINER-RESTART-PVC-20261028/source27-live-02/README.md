# Source27 remaining LIVE cases

These receipts bind the Source27 deployment to real Gov/Resource/PG/Kubernetes
business calls. No product source changed for these cases.

- `a26-lost-binding-01/`: new CPU and dedicated empty volume, one scoped PV
  binding fault, controller-produced Lost, rejected Attach with unchanged
  configuration/references, normal Storage deletion and automatic PV reclaim.
  This is an injected external binding fault, not natural CSI damage.
- `a26-lost-binding-01/03-a35-disabled/`: four formal disabled-name negatives
  with physical identity bookends and unchanged instance/references. This
  proves the plan's disabled-object alternative; historical DELETE convergence
  remains NOT_PROVEN.
- `a29-pvc-admission-race-01/`: all four admission/deletion ordering cases.
  The driver briefly holds an existing guard using SELECT FOR UPDATE and
  ROLLBACK. Actual lock-wait chains prove order; all business writes use the
  normal APIs. Gov CREATE ACK alone is not Resource admission. Each new volume
  generation and created instance is normally cleaned up. The original fixture
  CPU remains Stopped for the following recovery cases.
- `fault-build/`: separately identified Source27 plus task-only transport
  timing overlay. These build receipts do not establish any fault-case PASS.
- `a17-list-failure-live-01/`: preserved failed driver attempt. The original
  overlay did not match client-go's automatic timeout query. Its LIST fault and
  recovery outcome remain NOT_VERIFIED; the independent terminating-Pod sample
  proves that the actual Stop remained nonterminal while that Pod existed.
- `a17-list-failure-live-02/`: corrected transport matched the real complete
  LIST, received its real 2xx and dropped it. The same Stop remained nonterminal
  for two observations, then completed naturally after clearing the fault.
- `a17-label-negative-live-01/`: removal of one task Pod resource label made
  the label-only query empty while complete LIST and direct UID/name reads still
  found it. The same Stop remained nonterminal, the same Pod's label was
  restored with UID/RV tests, and the original Stop completed.
- `a15-unknown-start-live-root-01/`: preserved first timing attempt. The old
  Request.Cancel channel canceled the delayed original before transport; the
  requested ordering is NOT_VERIFIED. Both queued requests settled and the
  original normal DELETE completed. Formal product source did not change.
- `a15-unknown-start-live-root-02/`: all four real conditional-write orders,
  with the original Pending and one recovery fence preserved. Real status
  pairs were 200/409, 409/200, 409/dropped-2xx, and 409/409 after one task-only
  UID/RV-tested metadata update. All four original DELETE operations completed,
  Pending/Fence cleared, and their compute/workspace objects were gone.
- `resource-source27-fault-*`: direct diagnostic installation/update receipts,
  with separate immutable base/overlay/image identities. These are task-only
  timing builds; the formal Source27 image is the later restore target.
- `a30-storage-crash-live-02/response-drop/`: a real exact-UID Storage DELETE
  received Kubernetes 200, then lost that response. The same durable ticket
  retained write_started/unknown, observed actual absence and completed; the
  normal API returned GONE and the original PV was automatically reclaimed.
  This case did not exit the process and does not establish all A30 crash points.
- `a17-sts-negative-live-01/`: normal Start/Stop and original workspace proof
  passed. The brief STS negative window was not captured and remains
  NOT_VERIFIED; no extra Stop or controller change was used.
- `fault-build/04/`: Source27 plus exact post-commit process-exit hooks for a
  later scoped A14/A30 run. Build/deployment success is not a crash-case PASS.

Current progress and remaining required cases live only in
`docs/execution/status.md#container-restart-pvc-20261028`.
