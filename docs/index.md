# starci

CI defined as a script instead of YAML. A repository's `.ci.star` is a
[Starlark](https://github.com/bazelbuild/starlark) program whose `ci(ctx)`
function returns the steps to run. Because the graph is computed, it can follow
the inputs: test only the directories a commit touched, skip everything for a
docs-only change, fan out one step per package.

```python
def ci(ctx):
    build = step("build", image = "node:22", run = "make build")
    return [build] + [
        step("test-" + d, image = "node:22", run = "make test-" + d, needs = [build])
        for d in ctx.changed_dirs
    ]
```

There are thirty-nine worked examples in [`examples/`](examples/README.md): twenty-six basic and thirteen advanced.

## Status

Experimental. Runs execute on the machine `starci run` is on: a laptop, or a
job in another CI tool, which then does the triggering and the reporting.

- [x] **Planner** — evaluate `.ci.star` into a plan.
- [x] **Executor** — run a plan's steps in dependency order, each in a
      container that sees only the step's inputs, or [bare](bare.md) on the
      machine.
- [x] **Skipping** — reuse an earlier pass when a step's image, command and
      input contents are unchanged.
- [x] **Advanced mode** — a script that runs steps itself, sees their
      results, and branches or loops on them.
- [x] **Artifacts** — a step keeps files and a later step is handed them,
      by the hash of their content ([Handing files on](scripts.md#handing-files-on)).
- [x] **Driving from elsewhere** — `starci next` and `starci step` let
      another tool carry a run out and resume it ([Carrying a run out from
      another tool](driving.md)).
- [x] **Compiling** — write a basic script out as a GitHub Actions or
      CircleCI workflow, or as an executable that runs it.

Everything is one binary:

| | |
|---|---|
| `starci plan` | Evaluate a basic script and print the plan as JSON. Runs nothing. |
| `starci run` | Execute a commit on this machine, with Docker for steps in an image. |
| `starci compile` | Write a basic script out for GitHub Actions or CircleCI, or as an executable. |
| `starci validate` | Check that a script's inputs select files and that the tools it pins download as pinned. Runs nothing. |
| `starci next` | Say what a run does next, given its results so far. Runs nothing. |
| `starci step` | Run one step that `next` asked for, on this machine, and print its result. |
