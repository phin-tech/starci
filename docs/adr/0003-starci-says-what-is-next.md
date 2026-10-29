---
status: accepted
---

# starci says what is next; an engine runs it

An engine used to be handed a script and a callback: the script ran inside
the engine and waited in the callback for each step, so an engine that
resumes runs had to make Starlark survive a crash under its own replay rules.
Instead, starci is asked what is next. Given a run's script, its context and
the results so far, `Next` answers with the steps to run now, or that the run
is over and how it went, and keeps nothing between calls. An engine is a loop:
ask, run what it says, record the results, ask again. Whatever keeps the
results is the record a run resumes from, so every engine that keeps them is
durable without evaluating anything itself.

The same two things are on the command line, as JSON: `starci next` decides
and `starci step` runs one step on the machine it is on, printing its lines
as they come. Anything can drive starci with them (a Temporal workflow, a
Hatchet DAG, a loop in a CI job) without being a Go package or being loaded
by starci, which still names no tool. `engine.Engine` stays as it is for
engines in Go, the local one included, which build `Execute` from `Next` and
`step.Run` in-process, so every local run tests the loop.

## Steps are numbered

Each step a run asks for takes the next number, in both modes: in a basic
plan in the order the plan lists them, in an advanced script in the order it
asks, a parallel batch taking consecutive numbers in its order. A step is
known by its number, to the engine, in results, logs and the script; its name
is for people, and an advanced script may give two steps one name. With each
number `Next` records what was asked, and if evaluating again asks something
else at a number already answered, the run stops: the script is not the pure
function of its results that resuming depends on. A run's script is fixed by
its commit, so nothing else can move a number.

## Consequences

- An advanced script is evaluated from the top at every call, so the work
  grows with the square of its steps. That is nothing while Starlark only
  decides what runs; work belongs in a step.
- A basic plan no longer waits for a layer: a step is next once its own needs
  are done.
- Asking twice is harmless. `Next` returns every step wanted without a
  result, and an engine skips the numbers it has already started.
- Lines a step prints as it runs belong to whatever runs it, never to `Next`,
  which sees only finished results.
- A step's tracing `Carrier` travels with it, and its `Asked` time stays out
  of what is compared between evaluations.

## Considered options

- Keeping the callback and adding the command line beside it: two ways in,
  and the protocol tested only by whoever writes the first engine outside.
- Knowing steps by name and a count of repeats, as Inngest does, which lets
  a run survive its code being edited halfway; a commit cannot change, so
  numbers and the check are simpler and stricter.
- Keeping a script running between steps, as Temporal's sticky workers do:
  an optimisation to reach for only if evaluation is ever measured to matter.

## Later, and how

Designed but not built, because no engine that exists needs them:

- A run's record in the store, for an engine with none of its own
  (Kubernetes Jobs, Lambda, a loop in a CI job): each result written once,
  under the run and the step's number, only if absent, so of two machines
  racing on a step the first to finish is kept and the other takes its
  answer. The run id can come from the CI tool (`GITHUB_RUN_ID`). Killing
  the loser is the engine's business.
- `exclusive = True`, for a step that must never run twice at once: the
  machine claims it in the store before running and keeps the claim fresh,
  and one that finds a claim gone stale records the step as interrupted
  rather than running it again; a person decides. An engine honours it or
  says it cannot before the run starts. The local engine and the compiled
  workflows honour it already, since nothing in them runs a job again by
  itself.
- A lock across runs (one deploy to production at a time) is a different
  thing from `exclusive`, and waits for someone to need it.
