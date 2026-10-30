# Plan: `starci compile --target temporal`

**Status:** v1 is built (`internal/compile/temporal.go`, `starci sweep`), and
so are artifacts (`starci step` resolves placeholders from `given`). Not done:
tracing, the real-server test, and the kill-mid-step resume test.

## Decisions (agreed)

- **Frozen plan.** A basic script is evaluated at compile time and written out as a Go Temporal workflow. Advanced scripts are refused, as for the other targets. Anything that needs `Next` at run time is the Temporal *engine* candidate in AGENTS.md, not this.
- **Each activity runs `starci step`.** Services, bare steps, tools, passes and (later) tracing stay in `internal/step`. No third shell copy of `startServices` / `bareRun` / `artifacts.go`. Workers need starci and Docker.
- **Go output.** The generated module cannot import starci (`internal/`); a step is an opaque JSON blob handed to `starci step`. The Temporal SDK never enters starci's own `go.mod` (ADR 0001: starci only writes text).

## Facts checked in the code

| Question | Finding | Consequence |
|---|---|---|
| Does `starci step` resolve `artifact(step, path)` placeholders? | No. `resolve` lives in `internal/plan/artifacts.go` and only `Next` calls it. `stepJob` is `{step, context, clone_url, trace}`. | v1 refuses steps with placeholder artifacts (`checkPlaceholders` already exists). v2 adds a resolve input to `step`. |
| Does `starci step` call `step.Sweep`? | No. | A killed worker leaves services running. Generated `main.go` can't import `step`; either `starci step --sweep <machine>` / a `starci sweep` subcommand, or document the gap. Decide in v1. |
| Exit code of `starci step`? | 0 whenever it ran the step. Result JSON carries `status` (`passed`/`failed`/`skipped`). | Activity returns the result, not an error, for a failed step. Only non-zero exit is an activity error. |
| Is the per-event union reusable as data? | No: it is inline in `GitHubEvents` (`events.go`), interleaved with GitHub conditions. | Refactor first: extract "probe each event, merge steps, record which probes each step appears in" into a function both targets use. |

## Design

### Output

`starci compile --target temporal -o DIR` writes:

- `workflow.go`: the workflow and one activity.
- `main.go`: a worker (task queue(s), connection from env).
- `go.mod`: pinned Temporal SDK. Look up the current version when building; do not take it from memory.

`--check DIR` compares all three files. Golden outputs under `examples/basic/compiled/temporal/`.

### Workflow

- **Input:** `{event, sha, clone_url, context}`, i.e. what `starci next` would print as `context` and `clone_url`. Nothing is read from the clock or random sources.
- **Steps:** embedded as fixed JSON strings, numbered in plan order, so every replay hands Temporal byte-identical inputs.
- **Ordering:** `needs` becomes futures; independent steps run concurrently.
- **Failure:** a step whose dependency did not pass gets a `skipped` result without an activity, in the same shape `next` reports. The workflow's own result is `{ok, results[]}`.
- **Events:** per-event conditions from the extracted union, evaluated against workflow input. Same caveat as GitHub: values decided by file lists, non-default branches, tag names are frozen at the stand-in.

### Activity

- Runs `starci step` with the job JSON on stdin, parses the result from stdout, forwards stderr lines as heartbeats/logs.
- **Retry policy:** explicit; a failed step is a result and is never retried. Only "could not run" retries, a small bounded number.
- **Timeouts:** StartToClose set (configurable), heartbeat timeout set, heartbeat from a goroutine.
- **Cancellation:** on context cancel, SIGTERM `starci step`; `step` already handles it. Verify containers and services are removed (AGENTS.md requires it).
- **Placement:** `on` maps to a task queue named by label; `prefer`/`avoid` dropped and listed in the header, as CircleCI does. `os`/`arch` are well-known placement keys: honoured through queue naming or refused (`step.Unmet` on the machine is the backstop).
- **Bare:** `--bare` / `STARCI_BARE` come from the worker's environment, as for `starci step`.

### Refusals (v1, said in the header and at compile time)

- Advanced scripts.
- Steps handed artifacts (placeholders unresolved). `keep` alone is fine once a shared store is configured; steps that keep still need `STARCI_STORE` on workers and the header says so.
- A bare step with `network = False` (existing `bareRefusal`).

### v2

- `starci step` takes earlier results and resolves placeholders itself, reusing `resolve`; then artifacts work.
- Tracing: carry a `Carrier` from the workflow (trace id from the workflow id).
- Sweep at worker start.

## Work order

1. Extract the event-union from `GitHubEvents` into shared code; GitHub output must not change (existing golden tests).
2. Emit `workflow.go` / `main.go` / `go.mod` for a single-event plan; add `-o DIR` handling and `--check` to `compileCmd`.
3. Skip-on-failure and per-event branching.
4. Tests (below).
5. Docs: new section in `docs/compiling.md` (what carries over, what is refused), `docs/development.md` (how to run the gated test).

## Tests

- **Compile test:** generate into a temp dir with its own `go.mod` and `go build` it (skipped under `-short` if no module cache).
- **Workflow test:** Temporal `testsuite` with a stand-in activity, covering order, concurrency, skip after failure, failed step not retried, and event branching. Same idea as the stand-ins in `run_test.go`.
- **Real server:** gated behind `STARCI_TEST_TEMPORAL`; documented in `docs/development.md`. Runs a real `starci step` activity.
- **Kill-mid-step resume** (AGENTS.md): kill the worker partway through a step, start another, check the run resumes, finished steps are not repeated, and the run passes. A green suite does not show this.
- Goldens for `examples/basic/*` that are compilable.

## Settled

- **Sweep:** new `starci sweep MACHINE` subcommand; the generated worker runs it once at startup, before taking work.
- **Task queues:** one queue per sorted label set (`on=[gpu, linux]` -> `starci-gpu-linux`). A worker serving several sets listens on several queues.
- **Connection:** env only, via the SDK's standard client env config. Nothing about a cluster is baked into generated files.
