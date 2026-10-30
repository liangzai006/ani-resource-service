# Source27 invalid-volume Start recovery

`build/` and `deploy/` bind commit
`123e66c0daf6fdd946abada44d3ff9507d5127e4` to the static binary and deployed
image. Compilation took 24.048 seconds, build/publish 34.628 seconds and direct
deployment 9.547 seconds. No tests, generation or release gates ran for this
update, following the user's instruction.

`a27/` preserves the original Start29 ACK, its pre-fix accepted state and rejected
follow-up Stop, then the same Start's actual failed/done safe-stop result after
the update. Subsequent same-name/new-UID and remaining-invalid-volume Starts
also fail safely. Detach removes each selected invalid path without requiring
the other one to be valid. The workspace-only read uses the old marker, and
scratch volume cleanup uses normal Storage deletion after scoped fault steps.

`a22/` records the task Notebook's UID-preconditioned ordinary deletion, actual
garbage collection, Attach and Start safe failures without rebuilding compute,
release of unused references, and normal final instance deletion.

`a26-lost/` is NOT_VERIFIED: the status fault returned Lost, but the immediate
GET already returned Bound. No Lost-state Attach rejection was asserted. The
dedicated volume was normally deleted. This is not evidence of natural CSI
damage.

These receipts do not claim all A01–A41 fault/concurrency cases have passed.
Current execution and remaining work live only in `docs/execution/status.md`.
