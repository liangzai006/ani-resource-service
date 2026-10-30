# A17 replicas negative: one fresh actual sample

[Original state](state.safe.json), `negative_observations[0]`, is a fresh,
coherent LIVE negative proof for the original A17 requirement:

- Same Stop `de0627ed-f6d9-494f-8d55-a9ab06a209c6`, version24, is
  accepted/stopping in both official operation queries.
- Both Notebook GETs have the original UID and RV16052723, stopped=true/v24.
- The intervening actual StatefulSet GET has the original UID, replicas1,
  generation31/observed31. The instance is observed=pending and still names
  that same active operation. CurrentMounts[]/applied20 are unchanged.
- After the actual replicas became0 and the old Pod disappeared, the same Stop
  completed. Own temporary policy/binding were removed by UID.

The original driver required two such samples, which A17 does not require.
Only one was captured. Its later `NORMAL_STOP_FALSE_COMPLETED_WITH_EXACT_POLICY_ACTIVE`
error and `NOT_VERIFIED_NEGATIVE_NORMAL_STOP_CLEANUP_ONLY` result are preserved
as harness history; the later proper completion does not negate the first
coherent negative sample. They are not evidence of a product false-completion.

Result for the original assertion: **PASS**, limited to this actual sample and
normal completion. It does not prove that the temporary admission policy
caused the window or that an isolated internal guard branch was executed.
The earlier policy create was rejected because Namespace CEL metadata does
not expose UID; the same policy name was repaired before any binding existed.
The runtime Namespace UID check and exact original StatefulSet UID scope were
retained. No StatefulSet was directly patched.
