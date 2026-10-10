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
| `starci validate` | Check that a script's inputs select files and that the tools it pins download as pinned. Runs nothing. |
| `starci approve`, `refuse` | Answer a run that is waiting for a person. |
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
| `STARCI_DASHBOARD` | Hatchet's dashboard, as a person reaches it. The status a run posts then links to the run's page there, with each step's output and timing. Optional. |
| `STARCI_TRACES` | `true` to send traces to Hatchet too, where they appear on a run's page. Hatchet needs `SERVER_OBSERVABILITY_ENABLED=t`. Optional. See [Traces](#traces). |
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

### Schedules

A schedule is the one trigger nothing causes, so it is the one a script
declares:

```python
schedule("nightly", cron = "0 3 * * *")
schedule("backup", cron = "0 2 * * *", overlap = "replace", catch_up = True)

def ci(ctx):
    if ctx.event == "schedule" and ctx.schedule == "nightly":
        return [full_suite, audit]
    ...
```

| | |
|---|---|
| `cron` | Five fields, read in UTC. |
| `overlap` | What happens when it fires while its last run is still going: `"skip"` the new firing (the default), `"replace"` the run in progress, or `"allow"` both. |
| `catch_up` | `True` to run once if a firing was missed because nothing was running to fire it, however many were missed. Default `False`. |

**Only the default branch's schedules count.** Each push to it makes the
schedules that fire match what the script there declares: new ones are added,
changed ones updated, and ones no longer declared removed. A script that
cannot be read changes nothing, so a typo does not cancel tonight's run.

When a schedule fires, the worker looks at the default branch as it stands at
that moment, and asks the script there what the schedule wants done. The
result is posted on that commit as `starci/schedule-<name>`. A schedule whose
repository or script has gone removes itself.

On Hatchet a schedule is a Hatchet cron, visible in its dashboard. When
compiled for GitHub it becomes a `schedule:` trigger, where `overlap` and
`catch_up` do not carry over: GitHub lets scheduled runs overlap and never
catches up.

The record `catch_up` is measured against is kept on the worker that fires
schedules, so for now that is one worker's job.

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
labels, and so does `network = False`.

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

Before writing anything, `compile` downloads the script's tools for `--arch`
and checks them as `starci validate` does, so that a pin that no longer holds
is found here and not in every job. `--offline` skips that.

A compiled file is a copy of the script, and goes stale when the script is
edited without it. `--check` writes nothing and fails unless the file is what
would be written, which makes it a step worth running on every push:

```sh
starci compile --check .github/workflows/ci.yml
```

It says where the two first differ and how to write the file again. It needs
no network and downloads nothing.

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

### Engines

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
an interruption, placing steps by label, superseding, running one at a
time, and waiting for a person. What a run asks for that its engine cannot do is said before the run
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
| `Approver` | be told the answer to a gate a run is waiting at | `starci approve`, `starci refuse` |

Hatchet has all of them. The local engine has none, and one capability at
most: there is nowhere else to place a step, no other run to supersede, and
no record to resume from, but run at a terminal it can ask the person there
when a script waits.

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
| `step(name, image, run, needs = [], env = {}, inputs = ["."], caches = {}, cache = True, worker = {}, prefer = {}, avoid = {}, tools = [], packages = [], network = True)` | Declare a container to run. Returns a handle usable in another step's `needs`. |
| `ctx.repo`, `ctx.sha`, `ctx.ref` | What is being built. `ref` is the branch. |
| `ctx.event` | Why the run started: `"push"`, `"tag"`, `"pull_request"`, `"schedule"` or `"manual"`. |
| `ctx.tag` | The tag's name, for a tag. |
| `ctx.pr` | For a pull request: `.number`, `.base`, `.head`, and `.fork`, true when it comes from another repository. `None` otherwise. |
| `ctx.schedule` | The name of the schedule that fired. |
| `schedule(name, cron, overlap = "skip", catch_up = False)` | Declare a schedule, at the top of the file. |
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
| `wait_for(name, timeout = "24h")` | Stop until a person answers, and return the answer: `.approved`, `.by`, `.timed_out`. The name is letters, digits, spaces and `- _ . :`. |

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

### Waiting for a person

A script can stop and wait to be told to go on:

```python
def pipeline(ctx):
    run(build)
    run(tests)
    answer = wait_for("approve deploy", timeout = "4h")
    if not answer.approved:
        fail("not deployed: refused by " + answer.by if answer.by else "not deployed: nobody answered")
    run(deploy)
```

While it waits, the commit's status is pending and says what for, and the
run's log has the command that answers it, written out in full:

```sh
starci approve "approve deploy" --repo sam/app --sha 9c9be2c0… --event push
starci refuse  "approve deploy" --repo sam/app --sha 9c9be2c0… --event push
```

In a checkout of the repository at that commit, `starci approve "approve
deploy"` is enough for a run started by a push. A gate nobody answers within
its `timeout` comes back refused, with `.timed_out` set.

The wait is the engine's, not a worker's. A worker lost while a run waits
changes nothing: another evaluates the script again from the top, and every
step before the gate, and the gate, is answered from the record. The time a
run spends waiting is added to the time it is allowed. A newer push to
the branch replaces a run that is waiting, as it does any other.

What to know:

- **A gate is known by the repository, the commit, the event and its name,**
  not by which run it is. Two runs for the same commit and event that wait at
  a gate of the same name are answered together.
- **A waiting run keeps its place on its worker.** A worker carries twenty
  runs at once, counted apart from the steps it runs, and one that is waiting
  is one of the twenty. Steps are not held up by it; a twenty-first run on
  that worker is.
- **An answer is heard only by a run that is already waiting.** One given too
  early is not kept.
- **Who may answer is whoever can reach the engine.** `by` is what the person
  answering said, and defaults to their user name; nothing checks it.
- **`starci run --local` asks at the terminal,** if there is one. With none,
  it says before the run starts that the script waits and nobody is there,
  and the run fails when it reaches the gate.
- Only advanced scripts can wait. A basic script's steps are all known in
  advance and there is nowhere in it to stop.

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

A pin can be checked without running a step:

```
$ starci validate
inputs
  ok       test
tools
  ok       bun amd64
  failed   bun arm64
           tool "bun": https://…/bun-linux-aarch64.zip has checksum 5f0c…, not the a27f… the script pins it to
```

Every tool the script declares is downloaded, for every architecture it lists
whatever this machine is, and checked three ways: the address answers, what it
serves has the pinned checksum, and each `bin` directory is in it and holds a
program. It always downloads, since a copy this machine already has says
nothing about whether another could get one, and it works for both modes. It
sees tools declared at a script's top level and in `ci(ctx)`; one declared
only inside `pipeline(ctx)` is not known until that runs.

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

`starci validate` finds that error without running anything. It asks, of the
repository the script is in, whether each input selects a file that git
tracks: a file that was never added is not in the commit a worker builds. The
steps it asks about are those of a basic script's plan for a commit about
which nothing is known; a step added only for some branch, and every step of
an advanced script, is not known in advance and is not checked.

That boundary is what makes skipping safe. A step's result is stored under a
key made of its image's content id, its command, its `env`, its tools and
packages, whether it has the network, and the path and content of every file
its inputs select. When a later run produces the same key, the earlier pass stands in
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

### Steps without the network

A step can give the network up, which closes the largest of those gaps:

```python
step("unit", image = "golang:1.24", run = "go test ./...", network = False)
```

Its container is started with no network at all, only loopback, so a download
it did not declare fails the step the way a file it did not declare does. What
it needs has to arrive as an input, a tool, a package or the image. `caches`
still work, but a step that passes only because an earlier run filled one is
not sealed, so this suits steps whose dependencies are vendored or in the
image.

A pass with the network is never taken as a pass without it: turning the
network off runs the step once more. Tools and packages are fetched by the
worker before the container starts and are not affected.

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

The engine tests run against the local engine with everything else. Against
Hatchet they need one to talk to, so they run when asked, with this process
as the worker:

```sh
set -a; . dev/.env; set +a
STARCI_TEST_HATCHET=1 go test ./internal/engine/hatchet -run TestEngine
```
