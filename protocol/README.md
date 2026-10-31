# Local test runtime protocol

Version: `1`. The production daemon binds a literal loopback IP. All routes except
`GET /healthz` require `Authorization: Bearer <token>`. The owner token is created
in `<data-dir>/token` and never printed. Clients disable proxy and redirect
fallbacks. This transport does not provide Focus's execution authority.

## Endpoints

| Method | Path | Meaning |
| --- | --- | --- |
| GET | `/healthz` | Availability; evidence failure or quarantined cleanup returns 503 |
| GET | `/v1/capabilities` | Available adapter environment identities and admission budget |
| POST | `/v1/requests` | Validate a sealed execution descriptor; return Request/Job/Attempt identities |
| GET | `/v1/requests/{id}` | Current/final subscriber result with original execution identity |
| GET | `/v1/jobs/{id}` | Original job result, with no subscriber view overrides |
| POST | `/v1/requests/{id}/cancel` | Idempotently remove only that subscriber |
| POST | `/v1/jobs/{id}/references` | Retain original evidence using an opaque reference_id |
| DELETE | `/v1/jobs/{id}/references/{reference_id}` | Release only that reference |
| POST | `/v1/maintenance/collect` | Collect expired unreferenced settled evidence |
| GET | `/v1/maintenance` | Retained counts, journal bytes and last collection error |
| GET | `/v1/jobs/{id}/events?after_seq=N` | Strictly newer events, next cursor, terminal flag |
| GET | `/v1/requests/{id}/logs?stream=stderr&offset=N` | Up to 64 KiB of log text, byte cursor |
| POST | `/v1/fixture-jobs` | Backward-compatible unsealed built-in simulation submission |
| GET | `/v1/fixture-jobs/{id}` | Backward-compatible job summary |
| GET | `/v1/fixture-jobs/{id}/events` | Alias of the shared event reader |

JSON bodies are limited to 64 KiB and one object. Unknown fields, unsupported
versions, empty/repeated scope, invalid resource budgets and unsupported policies
are rejected. Stable errors contain `code` and `message`. Routing/method errors
retain standard HTTP semantics.

## Input sealing

Sealing is a local CLI operation, not an unauthenticated filesystem-reading HTTP
endpoint. `snapshot` produces a sorted manifest with exact file bytes, modes,
directories, declared exclusions and host path semantics. Limits are 10000
entries and 256 MiB. Default exclusions are `.git`, `.focus-test` and an internal
store directory. Ignored/untracked test inputs remain included.

Two scans plus per-file consistency checks reject observed changes. This proves
a verified copy of actual bytes, not an atomic filesystem revision. A Focus
adapter must provide its own cooperative write barrier. `materialize` creates a
private directory beneath the store and validates objects before copying.
After execution the original declared entries are checked again; generated
outputs are permitted, changes to declared inputs invalidate passing evidence.

## Execution descriptor

Use `focus-test plan` to construct a descriptor from `capabilities`. Its wire
type is `protocol.ExecutionRequest`:

- Protocol, caller-scoped idempotency key, opaque caller reference and trust domain.
- Sealed `snapshot_id`.
- Ordered test IDs, versioned adapter, ordered argv, workers and seed.
- Environment OS/architecture, executable hash, dependency/variable/data hashes.
- Execution timeout, isolation and network policy.
- CPU admission units, memory estimate and exclusive resource names.
- Independent request waiting timeout and reuse policy.

Environment variables are persisted as hashes. For real execution the provider
revalidates the current executable, script and inherited environment before
launch. Python pytest uses a controlled plugin and disables automatic third
party plugin loading. External installed-library closure is not established for
`node-v1` or `pytest-v1`; these adapters require `reuse_policy: none`.
The explicit `node-sealed-v1` profile supports inflight sharing for cooperative
self-contained CJS/ESM/JSON unit tests. It hashes its dependency guard, strips
host environment preloads, limits module resolution to sealed sources and a
builtin allowlist, and requires workers=1. See [the full input contract](../docs/node-sealed.md).
The host OS/runtime remains trusted; this is not a malicious-code sandbox.

The versioned execution key uses typed JSON: struct field order is fixed,
string map keys are sorted by Go's encoding/json, nil/empty optional collections
normalize, scope and argv order remain significant. It includes trust domain,
snapshot, plan, environment, policy and resource profile. It excludes
caller/request IDs, idempotency keys and request waiting deadlines. Calculating
a key verifies a descriptor, not execution or environment hermeticity.

## Sharing and cancellation

`fixture-v1` and the opt-in `node-sealed-v1` support `inflight`; `none` means
different requests create different jobs. No new request adopts completed
results. Same caller+domain+idempotency key and same descriptor returns the
original request, including after daemon restart. A changed descriptor conflicts.

One shared Job can have multiple Request identities. Each Request can cancel or
expire without affecting the rest. The last subscriber leaving cancels the Job.
A cancelling job leaves the sharing index immediately; its later finalization
cannot erase a replacement job's index entry.

CPU and memory are admission estimates. Resources are acquired together.
Requests are FIFO per caller and callers rotate; aged blocked work prevents
unbounded backfill. Worker count is part of execution identity, not a hard CPU
limit.

## Events and results

Events have contiguous per-job `seq`, `kind`, optional `test_id`, outcome and
bounded diagnostic. The reader is polling JSON, not JSONL streaming. Cursor zero
reads history; future/negative/malformed cursors are rejected. The journal's own
global sequence is separate from the public per-job sequence.

Event reads accept `limit=1..256` (default 128) and cap the encoded response at
256 KiB, including JSON escaping. A page may contain fewer than `limit` events.
`next_seq` is the last event actually returned, or the incoming cursor for an
empty page. `has_more` means another page is immediately available. `terminal`
becomes true only when the job has settled AND this page drains its retained
events. Continue from `next_seq`; never replace it with a job high-water mark.
The Go default reader is bounded too. Python `event_pages` drains available
pages; the 1 MiB transport response cap remains in force.

Real adapters use a separate JSONL file with strict version/sequence and a final
report closure. Early `case_failed` is diagnostic; final `case_finished` counts
the case once after phase accounting. Duplicate/malformed/incomplete reports,
missing requested scopes and zero cases cannot prove passing.

Node's execution-ordered `test:complete` drives early diagnostics. If process
isolation exposes a failed file before its named events, `group_failed` carries
that file's identity. It is a separate group observation, never a failed-case
increment. Consumers should notify on the first `case_failed` OR `group_failed`,
coalesce subsequent progress and retain final named case details. Complete and
ordered events for the same case are correlated, including older Node versions
that renumber forwarded `testNumber` values.

Final results include original Job/Attempt, requested snapshot, execution key,
exit code, report/cleanup status, scope and counts, retained workspace, diagnostic
and `result_digest`. The digest hashes the original job result before adding a
subscriber's Request ID or cancellation/expiry view overrides, with its own
digest field absent. Read `/v1/jobs/{id}` for that original result. It is an identity,
not a signature or standalone Focus authorization.

`simulation` is never application evidence. A real result uses `local-process`.
Passing requires a complete nonzero report, positive passing count, zero failure
and exit status, unchanged declared inputs, and confirmed process cleanup.
Skipped-only, cancelled, timed out, interrupted and infrastructure-error runs
remain inconclusive.

## Persistence and limits

Every transition is checksummed and synced before publication. The single state
directory is OS-locked. Corrupt/truncated journals fail startup; quota/write
failures stop publication. Retained requests are limited to 128, journal size to
64 MiB, and each record to 1 MiB. Automatic collection/compaction reclaims
unreferenced settled evidence; active, referenced and unknown-cleanup evidence
is protected. Idempotency ends when its retained request is evicted. Ordinary
writes reserve 2 MiB for cancellation/settlement; capacity backpressure does
not invalidate already committed records. See [retention semantics](../docs/retention.md).

Restart never replays unfinished jobs. They become interrupted/inconclusive.
Unknown cleanup on a real job quarantines new admission; an expired lease or
daemon restart is not proof that a process tree exited. Existing diagnostic
records remain available unless evidence itself is corrupt.

Output files are polled against a 4 MiB limit and checked again at termination.
Excess output cancels the process; a fast writer can overshoot the observed
threshold. Snapshot collection shares an OS operation lock with sealing and
materialization; newly sealed unbound inputs receive a configurable grace period.

Windows processes start suspended, join a kill-on-close Job Object, and resume
only after assignment. Cleanup waits for the active process count to reach zero.
POSIX uses process groups; Linux reaps adopted descendants. Local execution does
not prevent deliberate group escape or limit host file/network permissions.

## Focus boundary

`focus-broker` isolation is unsupported and rejected. See
`integration/focus/README.md` for the optional typed feedback middleware and the
still-required governed broker, workspace revision binding, ResultAdoption and
completion validation. The feedback probe records actual scripted-model input
and returned-response receipts at precise graph checkpoints. It does not modify
Focus's acceptance database or promote advisory results into completion evidence.
