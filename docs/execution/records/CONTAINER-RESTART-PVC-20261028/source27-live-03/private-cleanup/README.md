# Temporary credential and transport cleanup

[Remote receipt](remote.result.safe.json) records the normal close of three
proven task SSH masters (`private/db.sock`, `private/cluster-api.sock`,
`private-live-01/cluster-root.sock`) and removal of the task platform credential
and two DSN copies. [Local receipt](local-files.result.safe.json) records removal
of the matching local copies, task PG/registry password copies and bridge token.
The [new-host supplement](supplement-newhost.result.safe.json) removed the exact
task Docker auth copy. The [Fedora supplement](supplement-fedora.result.safe.json)
used one normal SSH connection to remove the two known task platform/bridge
copies. All four receipts have no skips and no business API, Kubernetes or SQL action. Original user
SSH keys, original attachments, platform roles/plans/quotas and workloads were
not changed.

Cleanup mistake: the first remote script also removed four old private session
diagnostic files (`error.private.json`, `events.private.json`,
`logout-1.private.json`, `own-session-identifiers.private.json`). Their removal
is retained in the actual receipt, rather than described as credential-only
cleanup. The script was corrected to preserve remaining private evidence.
The already committed [safe session result](../../self-session-cleanup-20261029-01.safe.json)
is retained, including the historical `NOT_CONFIRMED` revocation boundary.
No deleted private diagnostic content is reconstructed or claimed preserved.

The local transport close runs last, after the final normal Git push, so the
task SSH agent remains available for that push. The exact task `b.sock` master
is closed normally, then the proven `a.sock` task-agent process is terminated
with pidfd identity protection. The final execution receipt is retained outside
Git at
`/home/chabking/.codex/tasks/container-restart-pvc-20261028/final-private-cleanup-01/local-connections.result.safe.json`;
its existence/result, rather than this procedure text, proves actual closure.
Unknown processes are not killed and ownership drift is reported as a skip.
