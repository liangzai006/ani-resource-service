# A19 actual failure and mixed-template results

[Committed-target failure](postcommit-state.safe.json): Attach19
`986fbdeb-4971-4e60-947f-3c6b7f970dfe` actually committed the target/applied19
before its start write. The external fault normally deleted only the new,
dedicated scratch PVC by UID while the workload was stopped. The real start
write succeeded, then the original operation failed safely with
`INSTANCE_PVC_REFERENCE_CHANGED`. The instance became physically stopped,
Pending/Fence emptied, and the committed target/applied19 were retained.
Normal Detach20 and Storage cleanup repaired the configuration to empty.

[Mixed template](mixed-state.safe.json): the original Attach22
`07544ee2-2269-41c6-9950-c7b515f146ae` retained its original key/body/version.
The first intended injection window was not established: its delayed Stop
write actually returned409 after the Notebook resourceVersion advanced.
That failed harness result is preserved, and no successful template-PUT409
test is claimed for it.

Root instead conditionally added one new scratch volume and a different mount
path to this same task Notebook before that original Stop resolved. UID/RV and
the original two arrays were tested; owner references and other fields were
preserved. The initial slow-SSH CAS rejection is preserved; reuse of one task
SSH connection allowed the fresh UID/RV conditional patch to succeed.
All old queued writes were settled/cancelled before clearing the transport arm.

The same operation was then observed twice as blocked with
`INSTANCE_MOUNT_TEMPLATE_UNVERIFIED`, in `verifying_safe_stop`. The actual
workload was safely stopped; CurrentMounts stayed empty/applied20 and the new
reference remained pending. Restoring only the two original arrays allowed
that same operation to fail safely, with before configuration/applied20 intact,
Pending/Fence empty, and its one unused reference released. It did not complete
an Attach. The scratch was then normally deleted through Storage.

No product source, business-state SQL, direct StatefulSet write, new intent key
or forced deletion was used. The harness originally assumed the blocked phase
had to be applying/waiting-template and that recovery had to complete Attach;
the result above records the actual permitted safe-failure path instead.
