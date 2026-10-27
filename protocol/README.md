# Bootstrap simulation protocol

Protocol version: `1`. This is the **fixture simulation** contract, not the
snapshot-bound real execution protocol. It cannot start project tests or accept
shell commands. Every successful JSON response includes `mode: "simulation"`.

## Endpoints

| Method | Path | Response |
| --- | --- | --- |
| GET | `/healthz` | Health, protocol version, simulation mode |
| POST | `/v1/fixture-jobs` | HTTP 202, job identity |
| GET | `/v1/fixture-jobs/{job_id}` | Current or final result |
| GET | `/v1/fixture-jobs/{job_id}/events?after_seq=N` | Events after N, next sequence and terminal flag |

Request example:

```json
{
  "protocol_version": 1,
  "idempotency_key": "first-fixture-run",
  "test_ids": ["fixture.fail", "fixture.slow", "fixture.pass"]
}
```

Accepted IDs are `fixture.pass`, `fixture.fail`, and `fixture.slow`. They emit
simulated results in the specified order. Empty or repeated IDs, unsupported
versions, unknown fields, trailing JSON, and bodies larger than 64 KiB are
rejected. A request contains at most 32 IDs and a key of 1..128 bytes.

Same key plus same request returns the existing job. Same key plus different
request returns HTTP 409. Different keys create different jobs, even with the
same test IDs; cross-request merging has not been implemented.

## Events and results

Events contain `job_id`, a contiguous `seq` starting at 1, `kind`, and optional
`test_id`. Kinds are `job_queued`, `attempt_started`, `case_finished`,
`case_failed`, and `job_finished`. `case_finished` means this simulated case
passed. A failure event does not terminate the remaining simulated cases.

Cursor zero reads the full event history. Repeat reads are safe, and only
events strictly after the cursor are returned. A negative, malformed, or future
cursor is rejected. This endpoint is polling JSON, not a JSONL streaming API.

Job status is `queued`, `running`, `completed`, or `cancelled`. Outcome is
`inconclusive` until completion, then `passed` or `failed`; shutdown cancellation
remains `inconclusive`. Results include the requested IDs and completed/failed
counts. There is no application execution proof or adoption record.

Jobs and idempotency keys live only for this daemon process. Job IDs are random
to avoid confusing an old identifier with a new job after restart. At 128 total
retained jobs, new submissions return HTTP 503; retries of accepted jobs still
work. A daemon restart resets the simulation state.

Errors use JSON `{ "code": "...", "message": "..." }` for the implemented API
handlers. Routing errors such as unsupported methods use standard HTTP errors.
Requests and events are not durable; event visibility does not imply disk sync.

## Real execution boundary

The planned real execution contract must add caller/request identity, sealed
snapshot, environment manifest, exact test plan, resources, timeouts, reuse
policy, job/attempt identities, cleanup status and immutable evidence references.
Focus identity and permissions come from its service, not model-supplied fields.
The simulation API never produces evidence accepted by Focus completion checks.
