# Coordination benchmark

Run `go run ./cmd/focus-test-bench --output benchmarks/latest.json` from the
repository root. The command uses a private temporary directory and no external
service. It compares 1, 2, 4, and 8 simulated Contexts at the same admission
budget (2 CPU units, 512 MiB), with reuse disabled and with inflight reuse.

The output contains actual observed elapsed times and actual job counts. Inputs
are sealed before coordination timing; cold and warm content-store sealing
costs are reported separately. These are in-process simulated tests with one
measurement per cell. There is no model consumption measurement, application
test acceleration claim, persistent result cache, or measured CPU/memory usage.
Use this experiment to validate coordination mechanics, not to estimate ROI.

The measured engine uses in-memory events to isolate scheduling and merging;
durable journal sync latency is excluded. Cold/warm sealing describes an empty
versus populated content store, not a cleared operating-system disk cache.

Pass `--revision <git-commit>` to retain the tested source revision. The report
also records OS, architecture and Go version. Without the flag, revision is
explicitly `unrecorded`.

## Real Node and durable coordination

Run `go run ./cmd/focus-test-real-bench --repetitions 3 --revision <commit> --output benchmarks/real-node.json`.
This uses actual Node assertions with a concurrent one-second slow case and
an immediate failing case, node-sealed-v1, fixed 2 CPU / 512 MiB admission and
1/2/4/8 callers. Every caller seals its inputs. Completion time includes those
seals, validation, queueing, real execution, synchronous journal commits,
report and cleanup. Each cell has a new store; order alternates by repetition.
OS caches are not flushed. Failure time is the first durable event read by
the benchmark client after submissions, not an assertion timestamp.

Raw real samples are separate from the old simulation report. Three repeats
support descriptive median/range, not a stable tail or production ROI estimate.
Timer overlap exposes duplicate work; this is not an application benchmark.
Completed results are not cached. CPU time and RSS remain unmeasured.

`focus-feedback.json` is a separate experiment: actual upstream Focus graph
preparation/history, a scripted model, two independently routed Runs, one real
shared attempt, and failure changing the model's scripted next action while
the job is still running. It records exact parent checkpoints and response
hashes. Seal cost is separate; model times start at submission after graph
setup. Runtime/feedback storage is durable, graph checkpoints use InMemorySaver.
This is not installed production Patrol or acceptance evidence. See the
[reproduction instructions](../integration/focus/README.md).

Recorded Windows samples at `00e5518` (Node 24.14.0), medians of three runs:

| Callers | Jobs without reuse / shared | Complete without reuse (ms) | Shared (ms) |
| --- | --- | --- | --- |
| 1 | 1 / 1 | 1312.876 | 1312.915 |
| 2 | 2 / 1 | 1430.440 | 1347.647 |
| 4 | 4 / 1 | 2715.968 | 1352.944 |
| 8 | 8 / 1 | 5255.237 | 1347.306 |

This workload demonstrates actual execution reduction with sealing and fsync
included. At one caller, sharing has no advantage; gains depend on overlap and
duplicate load. These values do not predict production Focus acceleration.
The separate Focus graph sample observed its two returned-response calls at
285.8 / 317.8 ms after submission while the job was running; complete result
arrived at 3409.7 ms. A deliberately failed model call produced no response
receipt; retry produced a verified receipt. See raw JSON for all identities,
source revisions, limits and individual samples.
