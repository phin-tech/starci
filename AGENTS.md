# Working on starci

starci is CI whose pipelines are Starlark scripts. The README and the docs it
links to (`docs/`, built with MkDocs) say what it does and how to use it; this
file is for whoever changes the code. `CONTEXT.md` is the glossary, and
`docs/adr/` holds the decisions that would otherwise be undone by mistake.

```sh
go test ./...          # runs real containers; needs Docker
go test -short ./...   # without them
go vet ./... && gofmt -l .
```

Commits to this repository on git.phin.tech are authored as `bender` and pushed
through the `forgejo-bender` remote alias, never force-pushed.

## No tool is built in

starci runs steps on a machine, in an image or bare, and reports what
happened. It is not tied to an orchestrator, an environment manager or a
build system, and code that would tie it to one is the wrong shape (see
`docs/adr/0001-starci-names-no-tool.md`). A step that wants Flox, Nix or
make runs it as a command. Hatchet was the engine until it was taken out; git
history has it.

## Engines

starci decides what a run is. Something else gets its steps onto machines and
waits for them: an **engine**. There is one today, `internal/engine/local`,
which runs everything on the machine it is on, and the seam is kept for others
(Lambda, Temporal, DBOS, Kubernetes Jobs). Keep it that way, and add to it
only what an engine that exists needs.

### Where engine code may live

- `internal/plan` (the language, walking a plan) and `internal/step` (running
  one step on one machine) know no engine. Do not import an engine, or an
  engine's SDK, from either. `plan` asks a `Runner` to "run these steps
  together and hand back their results"; that is the whole seam.
- `internal/engine` holds the interface and nothing engine-specific.
- Everything specific to one engine goes in its own package under
  `internal/engine/<name>`, extras included: its dashboard links, its webhook
  receiver, its own tracing.
- `cmd/starci` asks an engine for what it needs through the interface. It
  should not reach into an engine's package for behaviour.

### The interface

`engine.Engine` is `Name`, `Can` and `Execute`, and nothing else is asked of
an engine yet.

- An engine says what it can do in `Can()`. What a run asks for that the engine
  cannot do is reported before the run starts (`Capabilities.Missing`). Never
  drop something silently, and never claim a capability the engine lacks.
- An engine with abilities beyond `Execute` (being handed a run and left with
  it, firing schedules, a fleet of machines to list) adds a small optional
  interface for each, asked for with a type assertion, shaped by that engine
  and changed when a second one does not fit. No plugin system and no
  registry: an engine is a Go package.
- What a machine is belongs to the engine. One that keeps long-lived
  machines (Hatchet called them workers) keeps its word for them inside its
  package; scripts, results and the CLI say machine.

### Adding an engine

1. Implement `engine.Engine` in `internal/engine/<name>`.
2. Put it through the shared tests: `enginetest.Run(t, eng)`. They use real
   containers and cover plan order, skipping after a failure, input
   sandboxing, result reuse, steps without the network, services, bare steps,
   advanced scripts, and whether the engine does what `Can()` claims. An
   engine is not done until it passes all that apply.
3. If the engine needs a service to test against, gate its test behind an
   environment variable (`STARCI_TEST_<NAME>`), and say how to run it in
   `docs/development.md`.

### Durable engines and replay

Learnt from Hatchet, and expected of any engine that resumes runs (Temporal,
DBOS). An advanced script is evaluated again from the top when the machine
carrying it out is lost, and each step it asks for must be answered from the
engine's record instead of being run a second time. This works because a
Starlark script asks for the same steps in the same order when given the same
answers. The engine side has to hold up its end:

- **What is handed to the engine for a step must be identical on every
  evaluation.** Hatchet refused to resume a run whose child inputs differed
  from the first attempt. Anything that changes between evaluations (a
  timestamp, a random id) must travel somewhere the engine does not compare.
- Cancelling a run must cancel its steps and remove their containers; an
  engine may not do that for child tasks on its own.
- Test it by killing a machine partway through a step of an advanced script and
  checking that the run resumes, finished steps are not repeated, and the run
  passes. A passing test suite does not show this.

### Services

A step's services (`plan.Step.Services`) are started and stopped by
`step.Run`, on the machine the step runs on, so an engine that runs steps
through `internal/step` has them for nothing. That is why there is no
capability for them: like `network = False`, every engine is held to them.

- An engine that starts a step's container some other way owes the same four
  things: each service answers to its name from the step, the step does not
  start until `ready` succeeds in the service's container, a step with
  `network = False` reaches its services and nothing else, and nothing of them
  is left when the step ends or is cancelled. A service's `files` are in its
  container, read-only, and not in the step's workspace.
- A machine that is killed leaves its steps' services running, and they do
  not end by themselves. `step.Sweep` removes what an earlier life of a
  machine left; an engine that keeps machines calls it as each starts. It goes
  by the machine's name, so call it only where that name belongs to one
  machine.
- The compile targets start services with the same Docker commands, written
  out as shell (`startServices` in `internal/compile`). A change to how a
  machine starts them needs making there too.
- For Kubernetes Jobs, unbuilt: services would be sidecar containers in the
  step's pod, `ready` a startup probe, and the names `hostAliases` for
  127.0.0.1. Two services on one port could not share a pod.

### Bare steps

A step with no image (`plan.Step.Bare`) runs as the machine's own processes.
`step.Run` does it, so an engine that runs steps through `internal/step` has
bare steps for nothing; like services, there is no capability for them.

- Only a machine that has agreed runs one (`step.Config.Bare`). `starci run`
  decides in `bareHere`: `--bare`, then `STARCI_BARE`, then yes except for a
  fork's pull request on a machine not known to be thrown away.
- `os` and `arch` are well-known placement keys. Every engine must honour or
  refuse them, and the machine checks them itself (`step.Unmet`).
- An engine that starts a bare step some other way owes what `runBare` does:
  a workspace of the inputs alone with the commit out of its reach, none of
  the machine's environment but its PATH, tools in front of it,
  `network = False` kept to or the step refused, and nothing it started left
  when it ends or is cancelled. A pass is reused only when the script asks
  (`cache = True`), keyed on the platform and the tools, not the machine.
- A bare step's processes are one process group, killed when the step ends
  or is cancelled. If starci is killed first, a watchdog kills them: starci
  run again as `__starci_watch`, holding one end of a pipe that the kernel
  closes when starci dies (`confine_unix.go`). So every program that runs
  bare steps calls `step.Watchdog()` before anything else in `main`, test
  binaries included (`TestMain`); otherwise the watchdog is that program
  doing whatever it does. A process that calls `setsid` escapes the group.
- The compile targets write the same thing as shell (`bareRun` in
  `internal/compile`). A change to how a machine runs a bare step needs making
  there too.

### Tracing

A run's trace is starci's own (`internal/tracing`), so every engine has one.

- An engine's only duty is to carry a `tracing.Carrier` with each step from
  whoever asks for it to the machine that runs it, call `Carrier.Step` around
  the step, and call `tracing.End` when the run is over.
- The trace id is made from the run's id, so a resumed run stays in one trace.
  Give `tracing.Ask` an id that is the same on every evaluation of the run.
- `Carrier.Asked` is a timestamp and falls under the replay rule above.
- An engine with tracing of its own may add it on top, inside its package. If
  the context a step is given is already inside a span, `Carrier.Step` nests
  there and `tracing.End` records nothing.
- Spans are exported by the standard `OTEL_EXPORTER_OTLP_*` variables. Do not
  add an engine-specific way to configure where traces go.

### Candidates, and what is known about them

Nothing below has been built or checked against the current SDKs. Verify
before relying on it.

| Engine | Shape | Likely fit |
|---|---|---|
| Lambda | A step is an invocation. | The step's image would be the function's image, or a bare step runs in a default one. Not durable on its own. Placement is architecture and memory; other labels are missing. |
| DBOS | A library plus Postgres; no server. | Durable scripts map naturally onto its workflows. No label matching, so `on`/`prefer`/`avoid` would become queues at best. No dashboard to link a status to. Confirm cross-process queues, cancellation, and recovery by another machine first. |
| Temporal | A server and long-lived workers. | Its Go SDK has an OpenTelemetry interceptor that handles propagation and replay; workflow code has strict determinism rules, which the replay section above already demands. |
| Kubernetes Jobs | A step is a Job. | Not durable on its own: there is no record to resume a script from. Placement maps onto node selectors. Tracing needs nothing but the `Carrier`. |

The local engine is the reference for "an engine with none of the
capabilities". A new one says in its package comment what it adds.
