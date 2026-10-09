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

Experimental. Plans are computed and executed against a local Hatchet; nothing
is deployed and nothing triggers a run automatically.

- [x] **Planner** — evaluate `.ci.star` into a plan.
- [x] **Executor** — run a plan's steps as Hatchet tasks, in dependency order,
      each in a container that sees only the step's inputs.
- [x] **Skipping** — reuse an earlier pass when a step's image, command and
      input contents are unchanged.
- [ ] **Deploy** — workers on real machines, with their own Hatchet tenant.
- [ ] **Trigger and report** — start a run from a Forgejo push webhook and post
      the result as a commit status.
- [ ] **Scripts as workflows** — let a script wait on a step's result and
      branch or loop on it, instead of returning a fixed plan up front.

Everything is one binary:

| | |
|---|---|
| `starci plan` | Evaluate a script and print the plan as JSON. Runs nothing. |
| `starci run` | Plan a commit of a local checkout and execute it on Hatchet. |
| `starci worker` | Execute steps on this machine. |

## Trying it

Needs Go, Docker and git.

```sh
go build -o starci ./cmd/starci

docker compose -f dev/docker-compose.yml -p starci-dev up -d   # throwaway Hatchet
dev/token.sh > dev/.env                                        # credentials for it
set -a; . dev/.env; set +a

./starci worker &
./starci run ~/src/pi-gamebook \
  --clone-url https://git.phin.tech/sam/pi-gamebook.git \
  --file examples/pi-gamebook.ci.star
```

`run` prints the plan's layers, waits, and reports each step:

```
sam/pi-gamebook @ e8da515a: 2 step(s)
  check  |  test

  passed   check  4.7s on mac
  passed   test  2.9s on mac

PASSED in 5.1s
```

The dashboard at http://localhost:18888 (admin@example.com / Admin123!!) shows
each run as a `ci-pipeline` parent with one `ci-step` child per step.

## Workers

A worker is the binary plus Docker, git, and a Hatchet token. It needs to
reach the Hatchet engine and the git server; nothing connects in to it.

| Variable | |
|---|---|
| `HATCHET_CLIENT_TOKEN` | Credentials for the Hatchet tenant. |
| `STARCI_WORKER_NAME` | Name recorded in results. Default: the hostname. |
| `STARCI_SLOTS` | Steps to run at once. Default: 4. |
| `STARCI_LABELS` | What the machine offers, as `gpu=true,arch=arm64`. |
| `STARCI_STATE` | Where passing results are kept. Default: `~/.cache/starci`. |

A step with `worker = {"gpu": "true"}` runs only on a worker carrying that
label, and waits until one is available.

## What a script can do

| | |
|---|---|
| `step(name, image, run, needs = [], env = {}, inputs = ["."], caches = {}, cache = True, worker = {})` | Declare a container to run. Returns a handle usable in another step's `needs`. |
| `ctx.repo`, `ctx.sha`, `ctx.ref` | What is being built. |
| `ctx.changed_files` | Files the commit changed, sorted. |
| `ctx.changed_dirs` | Their top-level directories, sorted (`.` for files at the root). |

Nothing else. A script cannot read files, reach the network, read the clock or
`load()` another file, and it is stopped if it runs too long. The same script
and inputs always give the same plan, so a plan can be computed in one place
and executed in another, and a script from any repository is safe to evaluate.

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
