# Source26 and new GPU evidence

These are byte-preserving copies of existing task receipts. Collecting them did
not issue business requests, query Kubernetes, rerun checks, or change deployment.
A26 and A05 CPU receipts are included below; the later A27/A22 and Source27
recovery receipts are in the adjacent `source27-live-01` directory.

- `build/` binds Source26 commit `964a7e14139015ea950a8dd8058011f82eb6aa34`
  to its source, static binary, OCI digest and published source labels.
- `deploy/result.safe.json` records the actual Source26 Resource deployment.
- `gpu/state.json` preserves the new GPU's original CREATE, read-only borrowed
  marker reads, logs and harness history. Mutable `first_pod_uid`/read fields are
  not the pre-Restart baseline; use the immutable old-ticket mint receipt.
- `gpu/terminal-cuda-direct-01/result.safe.json` is the Source25 terminal failure;
  `gpu/terminal-cuda-source26-01/result.safe.json` is the subsequent formal
  terminal, marker-read and CUDA success on the same version 2 and Pod UID.
- `gpu/old-ticket-mint-01/` freezes the unconsumed version 1/old-Pod ticket
  metadata; `gpu/old-ticket-reject-01/` records its rejection without runtime
  frames. Endpoint fields are templates; no ticket URL is included.
- `a14-a21/state.json` preserves original keys/bodies, ACKs, operation history,
  wrong-version rejection and DELETE cleanup bookends. `a14-resource-restart/`
  records the actual same-image Resource process restart used for A14.
- `ledger/` preserves the existing scoped original CREATE charge, before/after
  snapshots, single refund proof and their original hash manifest. Its stated
  limits remain: aggregate account equality and all seven recovery phases were
  not asserted; the pre-DELETE Network running binding and exact service-exit
  phase were not captured/proven.
- `cpu/` preserves the completed multipath/path/limit cases and the detach-only
  caller's running Detach, administrator restore and identity cleanup. The
  fixture manifest is an explicit safe projection, not the private source file.
- `network/` preserves the CPU version 25 durable/physical Network binding and
  its in-Pod Service DNS/TCP probe. This does not fill the absent GPU running
  Network receipt or establish public egress or all SG policies.

Private files, fixtures, credentials, stdout and raw ticket URLs were not copied.
