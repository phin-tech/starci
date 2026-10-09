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

There are thirty-one worked examples in [`examples/`](examples/), in both modes.

## Status

Experimental. Runs execute on this machine or against a throwaway Hatchet;
nothing is deployed and nothing triggers a run automatically.

- [x] **Planner** — evaluate `.ci.star` into a plan.
- [x] **Executor** — run a plan's steps as Hatchet tasks, in dependency order,
      each in a container that sees only the step's inputs.
- [x] **Skipping** — reuse an earlier pass when a step's image, command and
      input contents are unchanged.
- [x] **Trigger and report** — start a run from a push webhook (Forgejo or
      GitHub) and post the result as a commit status. Tested against stand-in
      forges; not yet against a real one.
- [ ] **Deploy** — workers on real machines, with their own Hatchet tenant.
- [x] **Advanced mode** — a script that runs steps itself, sees their
      results, and branches or loops on them.

Everything is one binary:

| | |
|---|---|
| `starci plan` | Evaluate a basic script and print the plan as JSON. Runs nothing. |
| `starci run --local` | Execute a commit on this machine, with nothing but Docker. |
| `starci run` | Execute a commit on Hatchet's workers. |
| `starci worker` | Execute steps on this machine, for Hatchet. |
| `starci serve` | Be the address a forge delivers webhooks to, relaying them to Hatchet. |
| `starci hooks add` | Have Hatchet itself be that address. |
| `starci compile` | Write a basic script out for GitHub Actions or CircleCI. |
| `starci workers`, `shun`, `unshun` | See the workers, and take one out of rotation. |

## Trying it

Needs Go, Docker and git.

```sh
go build -o starci ./cmd/starci

./starci run ~/src/pi-gamebook --local --file examples/basic/pi-gamebook.ci.star
```

```
src/pi-gamebook @ e8da515a: 2 step(s)
  check  |  test

  passed   check  2.9s on this machine
  passed   test  2.0s on this machine

PASSED in 2.9s
```

`--local` builds what is committed, fetched from the checkout itself, in the
same sandbox and with the same result cache as a worker. `-v` prints each
step's output as it runs; Ctrl-C stops the steps and removes their containers.
There is no queue and nothing to resume from: it is for trying a script, not
for running CI.

### On Hatchet

```sh
docker compose -f dev/docker-compose.yml -p starci-dev up -d   # throwaway Hatchet
dev/token.sh > dev/.env                                        # credentials for it
set -a; . dev/.env; set +a

./starci worker &
./starci run ~/src/pi-gamebook \
  --clone-url https://git.phin.tech/sam/pi-gamebook.git \
  --file examples/basic/pi-gamebook.ci.star
```

The dashboard at http://localhost:18888 (admin@example.com / Admin123!!) shows
each run as a `ci-pipeline` (basic) or `ci-script` (advanced) parent with one
`ci-step` child per step.

## Running on every push

Two things turn "something happened in a repository" into a run, and they are
kept apart on purpose.

```
forge ──▶ door ──▶ Hatchet ──event──▶ worker ──▶ the run
                                        │
          pass / fail on the commit ◀───┘
```

**A door** is the address the forge delivers its webhooks to. It relays the
delivery and does nothing else: it has no access to the forge, reads no
script, and can start nothing. So it can be almost anything, and several are
described in [`doors/`](doors/):

- nothing at all, with the forge pointed straight at Hatchet's own webhook
  address (`starci hooks add forgejo` sets that up);
- a function on a cloud provider: a Cloudflare Worker and an AWS Lambda are
  provided, about forty lines each;
- `starci serve`, the same thing as a process you run;
- anything that can forward an HTTP request.

**A worker** does the rest, as a task like any other. It checks the forge's
signature itself, fetches `.ci.star` as it is at that commit, evaluates it,
marks the commit `pending`, and starts the run. When the run ends it posts
`success` or `failure`. A script that cannot be evaluated is a failure on the
commit, with the message. A newer push to the same branch cancels the run
before it.

A worker acts on deliveries when it is given the forge's access token and
webhook secret:

| Variable | |
|---|---|
| `STARCI_FORGEJO_URL` | The instance, `https://git.example.com`. |
| `STARCI_FORGEJO_TOKEN_FILE` | File holding an access token: read repositories, write commit statuses. |
| `STARCI_FORGEJO_SECRET_FILE` | File holding the webhook secret. |
| `STARCI_GITHUB_TOKEN_FILE`, `STARCI_GITHUB_SECRET_FILE` | The same, for GitHub. |
| `STARCI_GITHUB_API` | The API's address, for GitHub Enterprise. Default: github.com. |
| `STARCI_ALLOW_FORK_PULL_REQUESTS` | `true` to run pull requests from forks. Off by default. |

With the token, a worker also fetches from private repositories; it is handed
to git through its environment and never appears in a clone URL.

There is no list of triggers to configure. Every event reaches the script,
which says in code what it wants done for it, including nothing.
`starci plan --event tag --tag v1.0.0` shows what a script would do for an
event without causing one.

| Event | The script is told | Status appears as |
|---|---|---|
| A push to a branch | `ctx.event == "push"`, `ctx.ref`, `ctx.changed_files` | `starci` |
| A new tag | `ctx.event == "tag"`, `ctx.tag` | `starci/tag` |
| A pull request opened or updated | `ctx.event == "pull_request"`, `ctx.pr` | `starci/pull-request` |

**Pull requests from forks are not run.** One is code from someone who cannot
push to the repository, and running it means running it on your workers. They
are ignored unless `STARCI_ALLOW_FORK_PULL_REQUESTS=true`; turn that on only
where the workers are meant for strangers' code.

Schedules a script declares are honoured when starci runs inside a CI tool,
and when compiled. Nothing fires them on Hatchet yet.

## Where starci is not running: compiling to a workflow

A basic script can be written out as a GitHub Actions workflow, which Forgejo
and Gitea Actions read too, or as a CircleCI configuration:

```sh
starci compile > .github/workflows/ci.yml
starci compile --target circleci > .circleci/config.yml
```

Each step becomes a job that does what a worker does: checks out only the
step's inputs, downloads its tools and checks their checksums, builds an image
with its packages, and runs its command in that container. A job that has
already passed with the same inputs skips the work, using the Actions cache as
the record. `needs` and `worker` carry over, as job dependencies and runner
labels.

A workflow file has to say in advance what happens for every event, so for
GitHub the script is asked once for each kind: a push to the default branch,
a push to another branch, a tag, a pull request, a pull request from a fork,
each declared schedule, and a manual run. A step that turns up for some and
not others becomes a job with a condition, and the events that lead to any
step at all become the workflow's `on:`. So

```python
if ctx.event == "tag":
    steps.append(publish)
```

compiles to a `publish` job that runs only for tags, and a script that will
not do something for a pull request from a fork still will not once compiled.

Some things cannot be frozen into a file, and the header of the generated
workflow says which apply:

- Anything decided by a particular value rather than by the kind of event: a
  rule about which files changed, about a branch other than "is it the default
  one", about what a tag is called. Those are frozen at what a stand-in gave.
- A step that is a different command for different events cannot be one job;
  compiling says so and asks for two names.
- The CircleCI output does not do this yet: it is the plan for a push.
- `prefer` and `avoid` are dropped. For CircleCI so is `worker`: it chooses
  machines by resource class, not by label.
- Advanced scripts cannot be compiled at all: their steps are only known as
  they run.

The CircleCI output keys a pass on the exact list of files the inputs select,
asked of git the way a worker asks, where the GitHub output has to make do
with globs. It uses the machine executor, since each job starts the step's
container itself.

The GitHub output has been linted and one of its jobs run by hand; the
CircleCI output is accepted by CircleCI's own validator and one of its jobs
has been run by hand. Neither has been run on the service it is for.

### Or run starci inside the CI tool

The other way is a workflow whose one job calls `starci run --local`. The CI
tool is then only the trigger. starci reads why the run started from the
tool's own environment (GitHub, Forgejo and Gitea Actions, and CircleCI are
recognised) and the script is told exactly what it would be told had starci
received the event: the tag, the pull request and whether it is from a fork,
the schedule that fired, the files a push changed. Nothing is approximated,
and advanced scripts work.

## How it fits together

| Package | |
|---|---|
| `internal/plan` | The language: evaluating scripts in both modes, and walking a plan. Knows no engine; it asks a `Runner` to "run these steps together and hand back their results". |
| `internal/step` | Running one step on one machine: fetch, sandbox, container, result cache. Knows no engine either. |
| `internal/engine/local` | A `Runner` that calls `step` directly, concurrently. |
| `internal/engine/hatchet` | A `Runner` that starts each step as a Hatchet task, plus the tasks themselves. |
| `internal/compile` | Writing a basic plan out for GitHub Actions or CircleCI. |
| `internal/ciinfo` | Reading why a run started from the CI tool starci is running inside. |
| `internal/forge` | Talking to Forgejo and GitHub: reading a push, fetching a script, posting a status. |
| `internal/trigger` | Turning a delivery into a run: the door that relays one, and the receiver that acts on it. |
| `doors/` | The contract for a door, and ones that are not Go: a Cloudflare Worker, an AWS Lambda. |
| `internal/store/s3` | The shared result store. |

An engine is whatever implements `Runner`. To run advanced scripts durably it
must also remember the results it handed back, so that a script evaluated
again after an interruption is given the same answers. Hatchet does; the local
engine does not, and says so.

## Workers

A worker is the binary plus Docker, git, and a Hatchet token. It needs to
reach the Hatchet engine and the git server; nothing connects in to it.

| Variable | |
|---|---|
| `HATCHET_CLIENT_TOKEN` | Credentials for the Hatchet tenant. |
| `STARCI_WORKER_NAME` | Name recorded in results. Default: the hostname. |
| `STARCI_SLOTS` | Steps to run at once. Default: 4. |
| `STARCI_LABELS` | What the machine offers, as `gpu=true,arch=arm64`. |
| `STARCI_STATE` | Where this machine keeps results and tools. Default: `~/.cache/starci`. |
| `STARCI_STORE` | A result store shared with other machines. Optional. |

### Placing steps

| On a step | |
|---|---|
| `worker = {"gpu": "true"}` | Only on a worker with that label. Waits until one is free. |
| `prefer = {"speed": "fast"}` | On such a worker when one is free, on any other when not. |
| `avoid = {"network": "home"}` | Never on a worker with that label. |

`starci run` warns before submitting when no connected worker can take a step.

### Looking after workers

```sh
starci workers          # who is connected, and what they offer
starci shun luigi       # finish what it has, take nothing new
starci unshun luigi
```

### Superseded runs

A run for a branch replaces the run before it: starting one cancels any still
in progress for the same repository and branch, and stops their containers.
Runs for different branches, or for a bare commit, leave each other alone.

## What a script can do

| | |
|---|---|
| `step(name, image, run, needs = [], env = {}, inputs = ["."], caches = {}, cache = True, worker = {}, prefer = {}, avoid = {}, tools = [], packages = [])` | Declare a container to run. Returns a handle usable in another step's `needs`. |
| `ctx.repo`, `ctx.sha`, `ctx.ref` | What is being built. `ref` is the branch. |
| `ctx.event` | Why the run started: `"push"`, `"tag"`, `"pull_request"`, `"schedule"` or `"manual"`. |
| `ctx.tag` | The tag's name, for a tag. |
| `ctx.pr` | For a pull request: `.number`, `.base`, `.head`, and `.fork`, true when it comes from another repository. `None` otherwise. |
| `ctx.schedule` | The name of the schedule that fired. |
| `schedule(name, cron)` | Declare a schedule, at the top of the file. |
| `ctx.changed_files` | Files the commit changed, sorted. |
| `ctx.changed_dirs` | Their top-level directories, sorted (`.` for files at the root). |

Nothing else. A script cannot read files, reach the network, read the clock or
`load()` another file, and it is stopped if it runs too long. The same script
and inputs always give the same plan, so a plan can be computed in one place
and executed in another, and a script from any repository is safe to evaluate.

## Two modes

A script is in one of two modes, told apart by the function it defines.

**Basic** — `ci(ctx)` returns the steps. The whole graph is known before
anything runs; `starci plan` prints it.

**Advanced** — `pipeline(ctx)` runs steps itself and reacts to what comes back:

```python
def pipeline(ctx):
    run(build)

    # How wide to fan out is read from a step, not written down.
    files = run(list_tests).output.splitlines()
    results = parallel([test(f) for f in files], allow_failure = True)

    # One more try for whatever failed, and only for that.
    failed = [r.name for r in results if not r.ok]
    if failed:
        results = parallel([test(n) for n in failed], allow_failure = True)
    if [r for r in results if not r.ok]:
        fail("failed twice")
```

| | |
|---|---|
| `run(step, allow_failure = False)` | Run one step and return its result. |
| `parallel(steps, allow_failure = False)` | Run several together; results come back in the same order. |
| `result.ok`, `.status`, `.output`, `.name`, `.seconds`, `.cached`, `.worker`, `.detail` | What a result carries. `output` is what the step printed, up to 64 KB. |
| `fail(message)` | End the run as failed. |

A step that fails ends the run, unless `allow_failure` hands the script the
failed result to decide on. Steps are the same `step(...)` as in basic mode,
with the same inputs, skipping and placement; `needs` is not used, since the
script's own order says what runs after what.

An advanced script runs on a worker, as a durable task. If that worker dies
partway, another picks the run up: the script is evaluated again from the top
and each step it asks for is answered from the record of the run, so finished
steps are not repeated. This is why the language matters. A script that cannot
read a clock or a file asks for the same steps, in the same order, whenever it
is given the same answers.

The trade is predictability. A basic script's graph depends only on the
commit; an advanced script's depends on what its steps return.

## Tools

A step can have programs put on its `PATH` instead of needing an image that
contains them, the way a `setup-*` action does, without downloading anything
it already has:

```python
bun = tool(
    "bun",
    url = "https://github.com/oven-sh/bun/releases/download/bun-v1.3.14/bun-linux-{arch}.zip",
    sha256 = {"amd64": "951ee2ae…", "arm64": "a27ffb63…"},
    arch_names = {"amd64": "x64", "arm64": "aarch64"},
    bin = ["bun-linux-{arch}"],
)

step("test", image = "debian:12-slim", tools = [bun], run = "bun test")
```

| | |
|---|---|
| `url` | The download: a zip, a tar archive, or the program itself. `{arch}` becomes the architecture the step's image runs on. |
| `sha256` | The download's checksum for `amd64` and/or `arm64`. An architecture not listed is not supported. |
| `arch_names` | How the publisher spells an architecture, where it differs. |
| `bin` | Directories inside the archive to put on `PATH`. Default: its top level. |

The checksum is the tool's identity. A worker keeps one copy per checksum,
downloads it once, and mounts it read-only into every step that asks. A step's
result is keyed on it, so changing a tool runs the step again and fetching the
same tool from a mirror does not. There is no way to ask for "the latest".

This suits programs that stand alone. A tool built against glibc will not run
in an Alpine image.

### Packages

For what a tool cannot carry — system libraries, a compiler — a step can ask
for apt packages on top of its image:

```python
step("test", image = "debian:12-slim", packages = ["libpq-dev", "git"], run = "...")
```

A worker builds an image from that base and those packages the first time a
step needs it, and reuses it for every step that asks for the same
combination. The image must be Debian or Ubuntu based.

Unlike a tool, a package is not pinned: it is whatever version apt offered on
the day that worker built the image, and it stays that version there until
the base image or the list changes. A step's result is keyed on the base image
and the list, so two workers that built at different times will share a result
even though their packages may differ slightly. Write `name=version` where
that matters.

## Inputs and skipped steps

A step sees only its `inputs`, by default the whole repository. Nothing else
is mounted, not even `.git`. An input is a file, a directory (everything
beneath it), or a pattern:

```python
BASE = ["src/**/*.py", "pyproject.toml", "uv.lock"]

step("unit", ..., inputs = BASE + ["tests/test_runs.py", "tests/conftest.py"])
step("gpu",  ..., inputs = BASE + ["tests/test_gpu.py"], worker = {"gpu": "true"})
```

In a pattern, `*` stays within one directory and `**` crosses them. Patterns
are matched against the commit when the step runs, so a new file that matches
becomes an input with no change to the script, and one that does not match is
never seen. An input that matches nothing is an error.

That boundary is what makes skipping safe. A step's result is stored under a
key made of its image's content id, its command, its `env`, its tools and
packages, and the path and content of every file its inputs select. When a later run produces the same key, the earlier pass stands in
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
Only passes are stored.

### Sharing results between machines

By default a machine knows only the results of steps it ran itself. Point
several at one store and a step that passed on any of them is skipped on all:

```sh
export STARCI_STORE='s3://starci-results/v1?endpoint=<account>.r2.cloudflarestorage.com'
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
```

Any S3-compatible bucket works; it has been run against Cloudflare R2. Each
machine still keeps its own copy and asks the shared store only for what it
does not have. The store is checked when a worker or a local run starts, so a
wrong address or credential is an error then, not steps quietly running again.
If it becomes unreachable later, steps run and their results are kept locally.

Results are keyed on the architecture as well as the image, so a pass on an
arm64 laptop is never taken as a pass on an amd64 server.

**Whoever can write to the store can make every machine skip a step**, by
storing a pass it never earned. Give its credentials only to machines you
trust to run steps honestly.

`caches = {"bun": "/root/.bun/install/cache"}` keeps a directory between runs
on a worker. It makes a step that does run faster, and is not part of the key.

## Development

```sh
go test ./...          # the step tests run real containers
go test -short ./...   # without them
```
