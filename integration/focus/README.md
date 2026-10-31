# Focus integration boundary

`client.py` is a standard-library authenticated local transport. The runtime is
not installed into Focus production. `focus-broker` remains unsupported and
is rejected; there is no automatic local-process fallback.

Focus still owns permissions, identity, revision fencing, the sealing barrier,
original execution records, ResultAdoption and CompletionSourceValidator.
Request/Job/Attempt identities must remain distinct. Do not relabel another
Run's execution or treat simulation or advisory local results as acceptance.
The adapter never writes Focus's database or bypasses existing completion checks.

## Optional real-graph feedback probe

`feedback_middleware.py` is an optional Focus graph extension, not an installed
desktop integration. The trusted Focus service registers each request against
its `SecurityContext` and expected runtime request identity. The token and
SQLite feedback journal must be outside the model workspace. Only exact
thread/namespace/Run/agent/task/workspace owners receive observations.

Before a model call, the middleware polls durable events. The first
`case_failed` or `group_failed` and final completion are separately journaled
and replay-deduplicated. Typed runtime observations are never direct user
instructions or forged collaborator messages. An exact saved checkpoint must
contain their typed source; the prepared request manifest and actual model
input must also include it. Only a returned model response writes a response
receipt. A prepared checkpoint or UI notification alone is not consumption.

Each request now has its own durable event cursor and drained-terminal marker.
Page validation, first-failure/final notices and cursor advancement commit in
one SQLite transaction. Failed commits replay from the old cursor; concurrent
stale readers refetch rather than overwrite newer progress. Completed streams
are not fetched again. Model preparation drains at most 32 pages per subscriber
per call and resumes any remaining tail next time. A saved notice survives a
crash between cursor advancement and graph checkpoint publication. Already
acknowledged retained notices are not reannounced; rollback can reinject a
notice absent from the restored history.

`verify_feedback.py` exercises upstream Focus's actual `WorldStateMiddleware`,
`TypedHistoryMiddleware`, security-context derivation and graph checkpoint
saver. Two independently routed graphs subscribe to one real node-sealed-v1
execution. A scripted local model chooses `continue_waiting` without feedback
and `inspect_failed_test` with failure; both failure calls assert that the slow
test is still running. One subscriber then cancels, while the other receives
the original complete failed result.

Reproduce with Focus at `853969375f9df46936552809dce3d543dd2403e4` and its
Python harness dependencies, from this repository:

```powershell
$env:FOCUS_TEST_UPSTREAM = 'C:\path\to\Focus'
$env:FOCUS_TEST_FEEDBACK_REPORT = 'D:\focus-test\benchmarks\focus-feedback.json'
go test ./cmd/focus-testd -run TestUpstreamFocus -count=1 -v
```

The test starts the authenticated daemon in process and always shuts it down.
Credentials remain in a private temporary directory. Source is sealed, Node
really executes assertions, and runtime/feedback journals synchronously persist.
The model is scripted; graph checkpoints are in memory; no model API is called.
Raw observations are in [focus-feedback.json](../../benchmarks/focus-feedback.json).

The optional middleware uses Focus's existing LangChain/LangGraph dependencies;
it adds none to the Go runtime or stdlib client. It polls at model boundaries
and does not wake idle Patrol Runs. It does not confine credentials from a
host-privileged model. The probe's source binding is explicitly advisory.
Production requires the governed broker, fenced source binding, durable Focus
installation, original execution / ResultAdoption records and completion
validator. Until then, local results cannot satisfy Focus completion criteria.
