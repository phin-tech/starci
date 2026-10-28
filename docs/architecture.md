# How it fits together

| Package | |
|---|---|
| `internal/plan` | The language: evaluating scripts in both modes, and walking a plan. Knows no engine; it asks a `Runner` to "run these steps together and hand back their results". |
| `internal/step` | Running one step on one machine: fetch, sandbox, container, result cache. Knows no engine either. |
| `internal/engine` | What starci asks of whatever carries a run out: the `Engine` interface, and the optional abilities beside it. |
| `internal/engine/local` | The engine that is this machine: it calls `step` directly, concurrently. |
| `internal/engine/hatchet` | The engine that is Hatchet: each step a task, a basic plan or an advanced script a durable one above them. |
| `internal/engine/enginetest` | What an engine has to get right, as tests every engine is put through. |
| `internal/compile` | Writing a basic plan out for GitHub Actions or CircleCI. |
| `internal/ciinfo` | Reading why a run started from the CI tool starci is running inside. |
| `internal/forge` | Talking to Forgejo and GitHub: reading a push, fetching a script, posting a status. |
| `internal/trigger` | Turning a delivery into a run: the door that relays one, and the receiver that acts on it. |
| `doors/` | The contract for a door, and ones that are not Go: a Cloudflare Worker, an AWS Lambda. |
| `internal/store/s3` | The shared result store. |
| `internal/tracing` | A run as an OpenTelemetry trace, whatever the engine. |

## Engines

starci decides what a run is: which steps, in what order, each given which
files. Getting those steps onto machines, waiting for them, and surviving a
machine going away is an engine's job. An engine has to do one thing:

```go
type Engine interface {
    Name() string
    Can() Capabilities
    Execute(ctx context.Context, r Run) (Result, error)
}
```

`Can` says which of the things a run may ask for the engine honours: surviving
an interruption, placing steps by label, superseding, and running one at a
time. What a run asks for that its engine cannot do is said before the run
starts, not dropped. `starci run --local` on a script with `worker` rules
prints which steps lost them.

Everything else is optional, and each ability is its own small interface that
a caller asks for with a type assertion:

| Interface | An engine that has it can | Used by |
|---|---|---|
| `Starter` | be handed a run and left with it | deliveries, schedules |
| `Scheduler` | fire the schedules a script declares | `starci worker` |
| `Host` | make this process a worker | `starci worker` |
| `Fleet` | list its workers, shun and unshun them | `starci workers`, `shun`, `unshun` |
| `Relay` | be passed a forge's deliveries, or receive them itself | `starci serve`, `starci hooks add` |

Hatchet has all of them. The local engine has none, and no capabilities
either: there is nowhere else to place a step, no other run to supersede, and
no record to resume from.

A new engine is a package that implements `Engine` and passes the shared
tests, which put real containers through basic and advanced runs and hold the
engine to what `Can` claims:

```go
func TestEngine(t *testing.T) { enginetest.Run(t, myEngine) }
```

The optional interfaces have only ever been shaped by Hatchet. Expect them to
move when a second engine with workers of its own is written.


## Traces

A run is one OpenTelemetry trace: a span for the run, and under it, for each
step, a `queued` span for how long it waited for a machine and a `step` span
with its parts inside (fetching the commit, identifying its inputs, finding
the image, looking for an earlier pass, running the command). Step spans say
whether the step was reused, where it ran and how it ended.

Set the standard variables and the spans are sent there, from `starci worker`
and from `starci run --local` alike:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.com   # HTTP; or
OTEL_EXPORTER_OTLP_PROTOCOL=grpc                       # for gRPC
OTEL_EXPORTER_OTLP_HEADERS=authorization=...
```

The trace is starci's own and needs nothing of the engine but to carry two
values with each step (`internal/tracing`). A run's trace id is made from the
run's id, not drawn at random, so an advanced script picked up again after its
worker was lost continues the same trace; the run's own span is recorded once,
when the run ends.

An engine may add to this. Hatchet does: with `STARCI_TRACES=true` it carries
the trace between tasks itself, and its collector becomes one more place the
spans go.
