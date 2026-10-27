# Focus integration boundary

`client.py` is an experimental standard-library transport client for a future
Focus-owned adapter. It implements authenticated submission, cursor reads,
result reads and subscription cancellation, with no proxy or redirect fallback.

The runtime has not been installed in Focus. The current `focus-broker` isolation
policy is rejected because the governed Focus execution broker does not exist
yet. Never substitute `local-process` automatically: it has the host user's
filesystem and network privileges.

Actual integration still requires changes in Focus's tool authorization,
workspace sealing barrier, execution lifecycle, typed test-event delivery,
model-consumption receipt, domain results and CompletionSourceValidator.
`request_id` belongs to the subscriber; `job_id` and `attempt_id` identify the
original execution. Bind the trusted Context/Run/call and workspace revision
outside the model. Preserve those original identities in ResultAdoption.

The standalone default data directory is inside the developer's workspace.
Focus must use an application-owned location outside model-readable trees and
apply its existing access policy; the standalone directory layout is not a
credential-isolation boundary for an agent with host file access.

Simulation results are always advisory. Local process results are also
insufficient for Focus acceptance until the dedicated source/adoption validator
checks authorization, exact scope, environment, source snapshot, complete
report and confirmed cleanup against the final workspace revision. The client
does not write the database or bypass any existing completion check.
