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

Experimental. A plan can be computed and executed against a local Hatchet;
nothing is deployed and nothing triggers it automatically.

- [x] **Planner** (Go, `cmd/starci`) — evaluate `.ci.star` into a JSON plan.
- [x] **Executor** (Python, `executor/`) — validate the plan with Pydantic and
      run its steps as Hatchet tasks, in dependency order, each in a container
      against a fresh checkout of the commit.
- [ ] **Deploy** — workers on real machines, with their own Hatchet tenant.
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
| `step(name, image, run, needs = [], env = {}, inputs = ["."], caches = {}, cache = True)` | Declare a container to run. Returns a handle usable in another step's `needs`. |
| `ctx.repo`, `ctx.sha`, `ctx.ref` | What is being built. |
| `ctx.changed_files` | Files the commit changed, sorted. |
| `ctx.changed_dirs` | Their top-level directories, sorted (`.` for files at the root). |

Nothing else. A script cannot read files, reach the network, read the clock or
`load()` another file, and it is stopped if it runs too long. The same script
and inputs always give the same plan, so a plan can be computed in one place
and executed in another, and a script from any repository is safe to evaluate.

## Running a plan locally

Needs Docker, Go and uv.

```sh
docker compose -f dev/docker-compose.yml -p starci-dev up -d   # throwaway Hatchet
dev/token.sh > executor/.env                                   # worker credentials

cd executor
uv run python -m starci_executor.worker &                      # a worker on this machine
uv run python -m starci_executor.trigger ~/src/pi-gamebook \
  --clone-url https://git.phin.tech/sam/pi-gamebook.git \
  --file ../examples/pi-gamebook.ci.star
```

The trigger prints the plan's layers, waits, and reports each step:

```
sam/pi-gamebook @ e8da515a: 2 step(s)
  check  |  test

  passed   check  4.0s on mac
  passed   test  2.1s on mac

PASSED in 5.1s
```

Two Hatchet tasks do the work (`executor/starci_executor/worker.py`):
`ci-step` runs one container, and `ci-pipeline` walks the plan one layer at a
time, starting a `ci-step` child for each step whose dependencies passed and
marking the rest skipped. The dashboard at http://localhost:18888 shows the
run as a parent with one child per step.

## Inputs and skipped steps

A step sees only its `inputs` — repository paths, by default the whole
repository. Nothing else is mounted, not even `.git`.

That boundary is what makes skipping safe. A step's result is stored under a
key made of its image's content id, its command, its `env`, and git's hash of
each input. When a later run produces the same key, the earlier pass stands in
and the step is not run:

```
  passed   check  cached, first passed on mac
  passed   test  cached, first passed on mac

PASSED in 1.0s
```

Because the key is content, not commit, this holds across commits: one that
only edits the README reuses the results of the commit before it. Narrower
`inputs` mean more reuse; a step that reads a path it did not declare fails on
the missing file rather than passing on a stale result.

What the key cannot see is anything outside the inputs: the network, the date,
a floating version resolved at run time. Give such a step `cache = False`.
Only passes are stored, and for now each worker keeps its own results.

`caches = {"bun": "/root/.bun/install/cache"}` keeps a directory between runs
on a worker. It makes a step that does run faster, and is not part of the key.

## Development

```sh
go test ./...
```
