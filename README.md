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

## Status

Experimental. Only the planner exists: it turns a script into a JSON plan and
runs nothing.

- [x] **Planner** — evaluate `.ci.star` into a plan (`starci plan`).
- [ ] **Executor** — run a plan's steps as Hatchet tasks, in dependency order,
      on whichever worker is free.
- [ ] **Trigger and report** — start a run from a Forgejo push webhook and post
      the result as a commit status.
- [ ] **Scripts as workflows** — let a script wait on a step's result and
      branch or loop on it, instead of returning a fixed plan up front.

## Using the planner

```sh
go run ./cmd/starci plan --file examples/pi-gamebook.ci.star \
  --repo sam/pi-gamebook --sha e8da515 --changed src/combat.ts,README.md
```

## What a script can do

| | |
|---|---|
| `step(name, image, run, needs = [], env = {})` | Declare a container to run. Returns a handle usable in another step's `needs`. |
| `ctx.repo`, `ctx.sha`, `ctx.ref` | What is being built. |
| `ctx.changed_files` | Files the commit changed, sorted. |
| `ctx.changed_dirs` | Their top-level directories, sorted (`.` for files at the root). |

Nothing else. A script cannot read files, reach the network, read the clock or
`load()` another file, and it is stopped if it runs too long. The same script
and inputs always give the same plan, so a plan can be computed in one place
and executed in another, and a script from any repository is safe to evaluate.

## Development

```sh
go test ./...
```
