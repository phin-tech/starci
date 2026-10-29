# How it fits together

| Package | |
|---|---|
| `internal/plan` | The language: evaluating scripts in both modes, and deciding what a run does next. Knows no engine; an engine asks it what is next, given the results so far, and runs what it says. |
| `internal/step` | Running one step on one machine: fetch, sandbox, container, result cache. Knows no engine either. |
| `internal/engine` | What starci asks of whatever carries a run out: the `Engine` interface. |
| `internal/engine/local` | The engine that is this machine: it calls `step` directly, concurrently. |
| `internal/engine/enginetest` | What an engine has to get right, as tests every engine is put through. |
| `internal/compile` | Writing a basic plan out for GitHub Actions or CircleCI, or as an executable. |
| `internal/ciinfo` | Reading why a run started from the CI tool starci is running inside. |
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
an interruption, and placing steps by label. What a run asks for that its
engine cannot do is said before the run starts, not dropped: `starci run` on a
script with `on` rules prints which steps lost them.

There is one engine, the local one, and it has neither: there is nowhere else
to place a step and no record to resume from. Hatchet was the other until it
was taken out (see the [decision](adr/0001-starci-names-no-tool.md)). A new
engine is a package that implements `Engine` and passes the shared tests,
which put real containers through basic and advanced runs and hold the engine
to what `Can` claims:

```go
func TestEngine(t *testing.T) { enginetest.Run(t, myEngine) }
```

An engine with abilities beyond running a run adds a small interface for
each, asked for with a type assertion.


## Traces

A run is one OpenTelemetry trace: a span for the run, and under it, for each
step, a `queued` span for how long it waited for a machine and a `step` span
with its parts inside (fetching the commit, identifying its inputs, finding
the image, looking for an earlier pass, running the command). Step spans say
whether the step was reused, where it ran and how it ended.

Set the standard variables and the spans are sent there from `starci run`:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.com   # HTTP; or
OTEL_EXPORTER_OTLP_PROTOCOL=grpc                       # for gRPC
OTEL_EXPORTER_OTLP_HEADERS=authorization=...
```

The trace is starci's own and needs nothing of the engine but to carry two
values with each step (`internal/tracing`). A run's trace id is made from the
run's id, not drawn at random, so an advanced script picked up again after its
machine was lost continues the same trace; the run's own span is recorded once,
when the run ends.

An engine may add tracing of its own on top, inside its package.
