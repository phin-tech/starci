# Working on starci

starci is CI whose pipelines are Starlark scripts. The README and the docs it
links to (`docs/`, built with MkDocs) say what it does and how to use it; this
file is for whoever changes the code.

```sh
go test ./...          # runs real containers; needs Docker
go test -short ./...   # without them
go vet ./... && gofmt -l .
```

Commits to this repository on git.phin.tech are authored as `bender` and pushed
through the `forgejo-bender` remote alias, never force-pushed.

## Engines

starci decides what a run is. Something else gets its steps onto machines,
waits for them and survives a machine going away: an **engine**. There are two
today, `internal/engine/hatchet` and `internal/engine/local`, and the design
assumes more: DBOS, Temporal, Kubernetes Jobs and the like. Keep it that way.

### Where engine code may live

- `internal/plan` (the language, walking a plan) and `internal/step` (running
  one step on one machine) know no engine. Do not import an engine, or an
  engine's SDK, from either. `plan` asks a `Runner` to "run these steps
  together and hand back their results"; that is the whole seam.
- `internal/engine` holds the interface and nothing engine-specific.
- Everything specific to one engine goes in its own package under
  `internal/engine/<name>`. That includes the engine's own extras: Hatchet's
  dashboard links, its webhook receiver and its trace collector are all in
  `internal/engine/hatchet`, not spread through the CLI.
- `cmd/starci` asks an engine for what it needs through the interface and its
  optional parts. It should not reach into an engine's package for behaviour.

### The interface

One thing is required (`engine.Engine`: `Name`, `Can`, `Execute`). Everything
else is a small optional interface, asked for with a type assertion: `Starter`,
`Scheduler`, `Host`, `Fleet`, `Relay`.

- An engine says what it can do in `Can()`. What a run asks for that the engine
  cannot do is reported before the run starts (`Capabilities.Missing`). Never
  drop something silently, and never claim a capability the engine lacks.
- The optional interfaces have only been shaped by Hatchet. When a second
  engine does not fit one, change the interface rather than bending the engine
  to imitate Hatchet. No plugin system and no registry: an engine is a Go
  package.

### Adding an engine

1. Implement `engine.Engine` in `internal/engine/<name>`.
2. Put it through the shared tests: `enginetest.Run(t, eng)`. They use real
   containers and cover plan order, skipping after a failure, input
   sandboxing, result reuse, steps without the network, services, advanced
   scripts, and whether the engine does what `Can()` claims. An engine is not
   done until it passes all that apply.
3. If the engine needs a service to test against, gate its test behind an
   environment variable the way Hatchet's is (`STARCI_TEST_HATCHET`), and say
   how to run it in `docs/development.md`.
4. Add optional interfaces only for what the engine really has.

### Durable engines and replay

An advanced script is evaluated again from the top when the machine carrying
it out is lost, and each step it asks for must be answered from the engine's
record instead of being run a second time. This works because a Starlark
script asks for the same steps in the same order when given the same answers.
The engine side has to hold up its end:

- **What is handed to the engine for a step must be identical on every
  evaluation.** Hatchet refuses to resume a run whose child inputs differ from
  the first attempt ("non-determinism error"). Anything that changes between
  evaluations (a timestamp, a random id) must travel somewhere the engine does
  not compare. For Hatchet that is the run's metadata; see `askedKey`.
  Expect DBOS and Temporal to have the same rule in their own form.
- Cancelling a run must cancel its steps and remove their containers. Hatchet
  does not do this for children on its own; see `cancelWithParent`.
- Test it by killing a worker partway through a step of an advanced script and
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
- A worker that is killed leaves its steps' services running, and they do not
  end by themselves. `step.Sweep` removes what an earlier life of a worker
  left; a durable engine's worker calls it as it starts, as Hatchet's does in
  `Engine.Work`. It goes by the worker's name, so call it only where that name
  belongs to one worker.
- The compile targets start services with the same Docker commands, written
  out as shell (`startServices` in `internal/compile`). A change to how a
  worker starts them needs making there too.
- For Kubernetes Jobs, unbuilt: services would be sidecar containers in the
  step's pod, `ready` a startup probe, and the names `hostAliases` for
  127.0.0.1. Two services on one port could not share a pod.

### Tracing

A run's trace is starci's own (`internal/tracing`), so every engine has one.

- An engine's only duty is to carry a `tracing.Carrier` with each step from
  whoever asks for it to the machine that runs it, call `Carrier.Step` around
  the step, and call `tracing.End` when the run is over.
- The trace id is made from the run's id, so a resumed run stays in one trace.
  Give `tracing.Ask` an id that is the same on every evaluation of the run.
- `Carrier.Asked` is a timestamp and falls under the replay rule above.
- An engine with tracing of its own may add it on top, inside its package.
  Hatchet's instrumentor is attached to starci's tracer provider in
  `Engine.Work`. If the context a step is given is already inside a span,
  `Carrier.Step` nests there and `tracing.End` records nothing.
- Spans are exported by the standard `OTEL_EXPORTER_OTLP_*` variables. Do not
  add an engine-specific way to configure where traces go.

### Candidates, and what is known about them

Nothing below has been built or checked against the current SDKs. Verify
before relying on it.

| Engine | Shape | Likely fit |
|---|---|---|
| DBOS | A library plus Postgres; no server. | Durable scripts map naturally onto its workflows. No label matching, so `worker`/`prefer`/`avoid` would become queues at best. Superseding, listing and shunning workers would be starci's own code. No dashboard to link a status to. Confirm cross-process queues, cancellation, and recovery by another machine first. |
| Temporal | A server and workers, like Hatchet. | Closest to Hatchet in shape. Its Go SDK has an OpenTelemetry interceptor that handles propagation and replay; workflow code has strict determinism rules, which the replay section above already demands. |
| Kubernetes Jobs | A step is a Job. | Not durable on its own: there is no record to resume a script from. Placement maps onto node selectors. Tracing needs nothing but the `Carrier`. |

The local engine is the reference for "an engine with none of the optional
abilities", and Hatchet for "an engine with all of them". A new one will land
somewhere between; say where in its package comment.
