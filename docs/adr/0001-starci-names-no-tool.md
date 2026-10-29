# starci names no tool

starci runs a script's steps on a machine, in an image or bare, and reports
what happened; it is not tied to any orchestrator, environment manager or
build system. Hatchet was its engine, and with it came workers, labels,
webhook receivers and durable runs as part of starci's model; it was taken
out, leaving the local engine and the `engine.Engine` seam for others, and
starci now runs on the machine it is on or inside another CI tool's job. Flox
was tried as a kind of step environment, and `uses` as a way to make bare steps
hermetic by hashing the programs they took from the machine; both were
dropped as making starci a build tool. A step that wants Flox or Nix runs it
as a command, an image is the way to say exactly what a step runs against,
and a bare step is honest about not being hermetic: it is reused only when
the script asks.

## Consequences

- No durable advanced scripts, schedules fired by starci, superseded runs,
  webhook-triggered runs or commit statuses until an engine that has them is
  written; inside a CI tool, the tool does the triggering and the reporting.
- What Hatchet called a worker is a machine everywhere: `on =` in place of
  `worker =`, `result.machine`, and the `machine` field of a stored pass. A
  script that says `worker =` is an error; a pass stored before is still
  found, without the name of the machine it ran on.
