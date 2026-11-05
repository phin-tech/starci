---
status: accepted
---

# An engine's SDK goes behind a build tag

An engine that drives a run itself, rather than starci writing a workflow out
as text, has to import its orchestrator's SDK. The first will be Temporal's,
and it brings gRPC, protobuf and a good deal of OpenTelemetry with it. Most
people who build starci run it on the machine they are on and want none of
that in their binary.

So an engine with an SDK is compiled in only when asked for, with a build tag
named after it (`-tags temporal`). Its package under `internal/engine/<name>`
carries the tag, and so does the one file in `cmd/starci` that makes the
engine. An untagged file beside that one has the same function and returns an
error that names the tag, so `--engine temporal` on a plain build says how to
get the engine instead of saying there is no such engine. The SDK is listed in
starci's own `go.mod`, and the plain binary does not contain it.

This does not change ADR 0001. The engine is a package beside the local one,
as AGENTS.md has always allowed, and starci still ties no step to a tool. The
compile targets still only write text. The module that
`starci compile --target temporal` writes pins its own SDK and never imports
starci.

## Consequences

- `go.mod` and `go.sum` list the SDK and what it needs, even though a plain
  build compiles none of it. `go mod tidy` keeps them, since it looks at every
  tag. A scanner that reads `go.mod` reports the SDK's advisories against
  starci, while `govulncheck`, which follows what is built, does not.
- Code behind a tag goes stale without anyone noticing. CI builds, vets and
  tests twice, once plain and once with every engine's tag, and the tagged
  pass is the one that runs the engine through `enginetest.Run`.
- gopls has to be given the tag (`buildFlags: ["-tags=temporal"]`), or it
  treats the engine as dead code.
- `go install -tags temporal git.phin.tech/sam/starci/cmd/starci@VERSION`
  installs the engine with no other module to keep in step.
- Each further engine with an SDK gets its own tag. A build with every engine
  is the tags together.

## Considered options

- **The SDK in `go.mod` with no tag.** This is simplest, but every binary
  carries an orchestrator that most people never run.
- **A module of its own in this repository** (`engine/temporal/go.mod`). Go
  allows a module whose path is under starci's to import `internal/`, so this
  would keep the SDK out of starci's `go.mod` altogether. It costs two modules
  held together with `replace`, which `go install …@version` ignores, and
  `go test ./...` would miss the engine from the root. The CLI would have to
  move out of `cmd/starci` into a package both binaries call, a second binary
  would be shipped and explained, and handing engines to that package edges
  toward the registry AGENTS.md rules out. If an empty `go.mod` ever matters
  for its own sake, the tagged package can be split off then.
- **No engine, only a driver outside the repository** that runs `starci next`
  and `starci step` (`docs/driving.md`). This remains open to anyone, but it
  gives no `Can()` and no shared engine tests, which are what make an engine
  trustworthy.
