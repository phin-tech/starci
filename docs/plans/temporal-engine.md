# Plan: a Temporal engine

**Status:** work order steps 1–3 are built (`internal/engine/temporal`,
`starci run --engine temporal`, `starci machine`), against SDK v1.50.0 and a
1.32 server. The workflow is tested under the SDK's test environment. The
shared engine tests against a real server, the placement split, the refusal of
artifacts without a store, and the durability tests are not done.

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

- **Determinism.** The workflow reads no clock and no randomness. A step's
  `tracing.Carrier` takes its `Asked` time from `workflow.Now`. Temporal does
  not compare activity inputs on replay; it uses the recorded ones. A
  timestamp in a step's input is therefore harmless, and Temporal would not
  notice a script that asks for something else either. `Next`'s check of
  each step's number and fingerprint is what catches that.
- **Workflow ID.** It is derived from the run, so the trace ID
  (`tracing.Ask(runID)`) is the same on every evaluation.
- **History.** Every `Next` call is handed every result so far, so history
  grows with the square of the steps. Pointers keep each result small. Measure
  before reaching for continue-as-new or a local activity for `Next`.
- **Script.** The workflow's input is the filename, the source, the
  `Context` and the clone URL. That is enough because `load()` is refused
  (`internal/plan/pipeline.go`) and a script reads no files. If `load()` is
  ever allowed, what it loads has to travel too.

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

- `starci machine --engine temporal` makes this computer a machine that
  takes steps from the engine. The command is generic, as the CLI says
  machine and never an engine's own word for one. `cmd/starci` asks the
  engine for an optional `Machine` interface by type assertion, as AGENTS.md
  describes for abilities beyond `Execute`. On a plain build,
  `--engine temporal` gives the build-tag message.
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

### One pool for every repository

The goal is one pool of machines (`starci machine --engine temporal`) that
serves every repository. Nothing on a machine belongs to a repository: each
step carries its clone URL and commit, and the script travels in the run's
input (`load()` is refused, so the source is the whole script).

- **Starting a run.** Each repository's CI job (a Forgejo Actions job, for
  example) runs `starci run --engine temporal`. The job works out the
  `Context` from its checkout (event, ref, changed files), starts the
  workflow, waits for it, prints each step's result, and exits with the run's
  status. The job needs no Docker, because steps in an image run on the pool.
  The CI tool still triggers runs and reports on commits, as ADR 0001 has it.
- **The pool evaluates the script.** The `Next` activity and the step activity
  both run on machines, so the starci that decides what runs is the pool's,
  not the one in the repository's job. What the job computes is only the
  `Context` and the workflow's input.
- **Versions.** Version skew comes down to two things. The workflow's input
  has to stay readable by the pool across versions. A fleet upgrade also
  replays runs already in flight on new workflow code, so either drain the
  fleet first or use Temporal's worker versioning. Run the replay tests
  before each upgrade.
- **Workflow ID.** It comes from the CI tool's run (repository, run id and
  attempt), so a job that is retried attaches to its run rather than
  starting a second one.
- **Credentials.** The pool has one set of git credentials
  (`step.Config.GitEnv`) that can read every repository it serves, and one
  shared `STARCI_STORE`. Passes are keyed on content, not repository, so
  repositories reuse each other's passes. That is deliberate.
- **Trust.** Every repository on one pool trusts the others. A bare step runs
  as the machine's user and could reach the store's credentials, so a
  repository you do not trust gets a pool of its own, with its own queues and
  store. A machine cannot use one `Bare` setting for every step. It has to
  apply `bareHere`'s rule to each step from the run's `Context`: no bare
  steps from a fork's pull request unless the machine is known to be thrown
  away.
- **Fairness.** Temporal's Task Queue Priority and Fairness does this on
  one shared queue
  ([docs](https://docs.temporal.io/develop/task-queue-priority-fairness)).
  Each step activity carries a fairness key, the repository (at most 64
  bytes, so hash it if it is longer), and the repositories are dispatched in
  proportion to their weights, so a busy one cannot starve the rest. A
  priority key, where lower goes first, can put pushes to the default branch
  and releases ahead of pull requests. Self-hosted, fairness is turned on
  with dynamic config (`matching.enableFairness`). Fairness does not reorder
  a backlog queued before it was on. When this was written the feature was
  in public preview, and fairness was planned as paid on Temporal Cloud.
  Check that the pinned Go SDK exposes both keys on `ActivityOptions`.

## Work order

1. Add `--engine` to `starci run`, the tagged file and the stub. CI builds
   both ways (see CI below).
2. Write the workflow, the `Next` activity, the step activity and `Execute`,
   and test them with the SDK's `testsuite` and a stand-in step.
3. Add `starci machine`, Sweep and Watchdog wiring, and task queues for
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

## Settled

- **Activity inputs on replay** are not compared. Temporal's documentation
  says an activity's arguments may change without breaking determinism. No
  Go-specific confirmation was found, so check again against the SDK version
  that is pinned.
- **`load()`** is refused, so the source is the whole script.
- **The machine command** is `starci machine --engine temporal`.
- **The server test's variable** is `STARCI_TEST_TEMPORAL_SERVER`, apart
  from `STARCI_TEST_TEMPORAL`, which needs only the network.
