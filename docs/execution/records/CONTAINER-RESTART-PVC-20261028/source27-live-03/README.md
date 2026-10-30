# Source27 fault recovery continuation

Product source remains `123e66c0daf6fdd946abada44d3ff9507d5127e4`.
The actual runtime for these cases is task overlay04, image
`sha256:c5292dc0a0cfd1ab0e2c141dd39ba4c4b2e70bb83c1202f76d1ccc31b3a648cd`,
based on that source; it is not represented as a pure Git tree.
The formal image was restored after these cases:
`sha256:1846ff442cc31f9a645f6137fb8a70ebc3f8d76e8a72bf675f0c72893714909e`.
Governance main/dev remains `a863ebc1639491bf4cf5c0acc02b0dbe9275a5ad`.
Current execution state is only in [status.md](../../../status.md#container-restart-pvc-20261028).

## A14: seven actual exits

[Raw state](a14/state.json) and [same-Pod status watch](a14/resource-pod-watch.safe.jsonl).
One original Attach operation `0c6375c4-1916-4b2d-a8c3-6be2d0a23ec5`,
instance `9b2e5371-5be3-4cac-8766-55147d8041fb`, version14, completed with
`runtime_verified`, applied14 and actual Running. The original workspace marker
was read without writes, the workload Pod changed, and the new PVC was proved
Bound and mounted using the real container mount table.

All seven exact post-commit stage markers belong to that original operation.
The same Resource Pod's restart count advanced from0 to7. Kubernetes status
captured five distinct exit86 witnesses; it did not capture every phase's exit
individually. The final PG read has Pending/Fence null. The matching stage arm
was cleared, followed by normal Stop/Detach/Storage deletion of the new scratch.

The monitor initially failed to parse a Go RFC3339Nano deadline. The driver was
fixed to accept 1–9 fractional digits, retaining the original30-minute deadline,
key and request body. Two earlier monitoring intervals ended on this harness
error; the final watch's empty gap array does not erase those earlier gaps.
No new Attach operation, lease change or business-state SQL write was used.
The roughly12-minute operation includes Kubernetes' increasing restart delay.

## Other cases

- [A18 owner and GPU safety evidence](a18/README.md).
- [A17 actual replicas1 negative sample](a17-native-replicas/README.md).
- [A17 natural STS window observation](a17-natural/README.md) and
  [observed-generation temporal proof](a17-natural/observed-lag-result.safe.json).
  The actual watch lag followed by the same operation still accepted, together
  with terminal monotonicity, proves noncompletion during that lag. It does not
  claim a coincident fresh GET or an isolated internal guard branch read.
- [Uncaptured later admission windows](a17-native-observed-attempts/README.md)
  remain `NOT_VERIFIED`; original harness errors are preserved.
- [A19 committed-target and mixed-template safety failures](a19/README.md).
- [A30 actual process-loss and old-generation recovery](a30/README.md).
- [Final CPU deletion and scratch reclamation](final-cpu/README.md).
- [Formal Source27 restore, 10.525 seconds](formal-restore/README.md).
- [Temporary credential and transport cleanup, including the documented mistake](private-cleanup/README.md).

All task business objects in these cases were cleaned through normal APIs.
Original borrowed PVC/PV identities and data were retained. A36 remains
`DEFERRED_USER_SCOPE`; A35 used the approved disabled-object alternative and
does not establish the old DELETE history (`NOT_PROVEN`).

No product source change, build, generated output, test suite or release gate
was added for this evidence update. Required live business fault cases are
recorded separately from publication readiness.
