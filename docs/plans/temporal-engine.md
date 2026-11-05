# Plan: a Temporal engine

**Status:** not started. The packaging is decided
(`docs/adr/0004-an-engine-sdk-behind-a-build-tag.md`), and so is the shape of
the loop (ADR 0003). Nothing below has been checked against the current
Temporal Go SDK; look up its version and behaviour when building, not from
memory.

This is not `starci compile --target temporal` (`docs/plans/temporal-target.md`).
That target freezes a basic plan into a generated module and refuses advanced
scripts. This engine is `internal/engine/temporal`: it implements
`engine.Engine`, asks `Next` at run time, and runs both kinds of script, keeping
an advanced one's place across the loss of a machine.

## Decisions (agreed)

- **Build tag `temporal`** (ADR 0004). The SDK is in starci's `go.mod`, and a
  plain build neither compiles it nor contains it.
- **One CLI.** A tagged file in `cmd/starci` makes the engine, and an untagged
  stub returns "built without the Temporal engine; rebuild with
  `-tags temporal`".
- **Starlark never runs in workflow code.** `Next` is an activity, and its
  answers are kept in history.

## Design

### Workflow

A loop with the shape of `local.drive`:

1. Run the `Next` activity with the run's context, clone URL, script and
   results so far, and get back a `plan.Decision`.
2. Start a step activity for each `Asked` whose number has not been started.
3. Wait on a selector for any one of them, then record
   `asked.Answer(result)` so the number and fingerprint are kept.
4. Repeat until `Decision.Done`. The workflow's result is
   `engine.Result{OK, Steps, Error}`.

- **Determinism.** The workflow reads no clock and no randomness. A
  `tracing.Carrier` is made inside the activity, or through
  `workflow.SideEffect`.
- **Workflow ID.** It is derived from the run, so the trace ID
  (`tracing.Ask(runID)`) is the same on every evaluation.
- **History.** Every `Next` call is handed every result so far, so history
  grows with the square of the steps. Pointers keep each result small. Measure
  before reaching for continue-as-new or a local activity for `Next`.
- **Script.** The workflow's input carries the script's source. If a script
  can `load()` other files, the input carries what it loads as well. Check
  this first.

### Step activity

- Runs `step.Run` in the worker process (it is a starci binary), with
  `step.Config` taken from the worker's environment, as `starci step` takes it.
- Heartbeats from a goroutine while the step runs. When the context is
  cancelled, the step's containers and services go, and so do a bare step's
  process group. The engine tests check this.
- **Retries.** A step that ran and failed is a result and is never retried.
  Only "could not run" is retried, a few times. Take the timeouts and retry
  counts from the compile target's tuning constants.

### Worker

- `starci worker` (name to be settled) is a tagged command. To start it,
  `cmd/starci` asks the engine for an optional interface by type assertion,
  as AGENTS.md describes for abilities beyond `Execute`.
- It calls `step.Watchdog()` first, which `main` already does, and
  `step.Sweep(machine)` before taking work. The machine name must belong to
  that worker alone.
- The connection comes from the SDK's standard environment config, and
  nothing about a cluster is a starci flag.

### Capabilities

- **`Durable: true`,** claimed only once the kill-mid-step test below passes.
- **Placement.** `on` maps to a task queue per sorted label set, as in the
  compile target. `prefer` and `avoid` have no equivalent. `Capabilities.Placement`
  is one bool, so split it (for example `Placement` for `on`, and `Preference`
  for `prefer` and `avoid`) rather than claim what is not done.
  `Capabilities.Missing` and the local engine change with it.
- **Artifacts** need a shared `STARCI_STORE`. A run that keeps or is handed
  artifacts without one is refused before it starts.

### Execute and the CLI

- `Execute` starts the workflow, waits for it, and returns its result.
- `starci run --engine local|temporal`, with `local` the default. There is no
  `--engine` flag yet.
- **Step output.** A step's lines print on the worker, not where
  `starci run` was typed. v1 shows each step's output when it ends, from the
  result. Streaming lines live is later and needs its own decision.

## Work order

1. Add `--engine` to `starci run`, the tagged file and the stub. CI builds
   both ways (see CI below).
2. Write the workflow, the `Next` activity, the step activity and `Execute`,
   and test them with the SDK's `testsuite` and a stand-in step.
3. Add the worker command, Sweep and Watchdog wiring, and task queues for
   `on`.
4. Pass `enginetest.Run` against a dev server.
5. Split the `Placement` capability, and add the refusal for artifacts without
   a store.
6. Write a replay test with `worker.WorkflowReplayer` on a recorded history,
   and the kill-mid-step test. Claim `Durable` only after both pass.
7. Docs: `docs/development.md` (the tag, gopls, how to run the tests),
   `docs/architecture.md`, the package comment, and AGENTS.md's table of
   candidates.

## Tests

- **Unit:** the workflow under `testsuite` with a stand-in step activity,
  covering order, concurrency, skipping after a failure, a failed step not
  being retried, and an advanced script's branches.
- **Engine:** `enginetest.Run(t, eng)` against `temporal server start-dev` or
  the SDK's dev server. It needs Docker and a server, so it is gated behind an
  environment variable. `STARCI_TEST_TEMPORAL` already gates the compile
  target's module test, which needs only the network. Either give the engine
  its own variable (for example `STARCI_TEST_TEMPORAL_SERVER`) or have one
  variable mean "Temporal tests may use the network and a server". Decide this
  when writing it.
- **Replay:** histories recorded from real runs, replayed against the current
  workflow code, to catch a change that is not deterministic.
- **Kill mid-step** (AGENTS.md): kill a worker partway through a step of an
  advanced script and start another. The run must resume, finished steps must
  not run again, and the run must pass.

## CI

The repository has no CI of its own for Go yet (`.github/workflows` builds
only the docs). Once it has some, every check runs twice:

```sh
go vet ./... && go test -short ./...
go vet -tags temporal ./... && go test -short -tags temporal ./...
```

The full engine test runs with the tag and the server variable set, on a
machine with Docker.

## Open questions

- Does the Go SDK compare activity inputs on replay? If it does,
  `Carrier.Asked` has to travel outside the activity's arguments.
- Do scripts `load()` other files? If they do, what travels in the workflow
  input?
- Should the worker command be `starci worker`, or something under the engine
  such as `starci temporal worker`?
