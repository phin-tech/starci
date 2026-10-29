# Where starci is not running: compiling to a workflow

A basic script can be written out as a GitHub Actions workflow, which Forgejo
and Gitea Actions read too, as a CircleCI configuration, or as a Temporal
workflow:

```sh
starci compile > .github/workflows/ci.yml
starci compile --target circleci > .circleci/config.yml
```

Each step becomes a job that does what a machine does: checks out only the
step's inputs, downloads its tools and checks their checksums, builds an image
with its packages, and runs its command in that container, or on the runner
itself for a [bare step](bare.md). A job that has
already passed with the same inputs skips the work, using the Actions cache as
the record. `needs` and `on` carry over, as job dependencies and runner
labels, and so does `network = False`.

[Artifacts](scripts.md#handing-files-on) carry over too. A job that keeps
something hands it on through the CI tool's own way of moving files between
the jobs of a run (Actions artifacts, packed with tar so that executable files
stay executable, or CircleCI's workspace), and keeps it in its record of a pass
as well, so that a job skipped because it passed before still hands on what it
kept then. A job handed something is keyed on a hash of its content, worked out
the same way a machine does: if the job before it runs again and makes the
same files, it is skipped.

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
- `prefer` and `avoid` are dropped. For CircleCI so is `on`: it chooses
  machines by resource class, not by label.
- A step's services are started by the job itself, the way a machine starts
  them, and stopped at its end. They are not the CI tool's own `services:`,
  which could not be reached by name from the step's container. A pass is
  keyed on a service's image by name: a tag that moves does not run the job
  again, as it would on a machine.
- Advanced scripts cannot be compiled to a workflow at all, or frozen into an executable: their steps are only known as
  they run.

Every script in `examples/basic` is compiled both ways under
`examples/basic/compiled`.

The CircleCI output keys a pass on the exact list of files the inputs select,
asked of git the way a machine asks, where the GitHub output has to make do
with globs. It uses the machine executor, since each job starts the step's
container itself.

The GitHub output has been linted and one of its jobs run by hand; the
CircleCI output is accepted by CircleCI's own validator and one of its jobs
has been run by hand. Neither has been run on the service it is for. The
tests carry out workflows of bare steps that hand artifacts on with stand-ins
for both tools (a job's shell run as it is, checkout, cache, artifacts and
workspace done with directories), twice, so that the second time jobs are
skipped by the passes of the first.

## Or a Temporal workflow

```sh
starci compile --target temporal -o ci-workflow
cd ci-workflow && go mod tidy
```

writes a small Go module: `plan.go` (the plan, asked once for each kind of
event as for GitHub), `workflow.go` (`StarciRun`, which runs each step as an
activity once the steps it needs have passed), `activity.go` and a worker in
`main.go`. The activity does one thing: it runs `starci step` for the step, so
services, [bare steps](bare.md), tools and passes are what they are everywhere
else. A worker is a machine with starci and Docker on it.

```sh
STARCI_QUEUES=starci go run .      # on each machine; connection from TEMPORAL_*
temporal workflow start --task-queue starci --type StarciRun --workflow-id RUN \
  --input "$(starci next . </dev/null | jq -c '{context, clone_url}')"
```

The input is what `starci next` prints the first time: the run's context and
clone URL. Which steps run is decided from the context, so a schedule is a
Temporal Schedule that starts the workflow with `event` set to `schedule` and
`schedule` to its name.

- A step that ran and failed is a result, never retried; steps that need it are
  skipped. Only a step starci could not run at all (a lost machine, no Docker)
  is tried again, three times, before it too is a failed result. The run's
  output is `{ok, results}`.
- `on` chooses the task queue: a step that asks for `{"gpu": "a100"}` goes to
  `starci/gpu=a100`, and a worker takes it by listing that in `STARCI_QUEUES`
  (comma-separated). `prefer` and `avoid` are dropped.
- The worker runs `starci sweep` as it starts, removing the services a step of
  a killed worker left running. It goes by the host name, so run one worker to
  a host.
- Cancelling the run signals each `starci step`, which takes its containers
  down.
- Steps that are handed [artifacts](scripts.md#handing-files-on) cannot be
  compiled yet: only `starci next` puts real pointers in place of `artifact()`. A step
  that keeps files needs `STARCI_STORE` on every worker.
- Advanced scripts cannot be compiled; something that asks `starci next` can
  carry them out ([driving](driving.md)).
- The attempts, the timeouts and the SDK version are starci's choices, written
  in the files. `--check DIR` compares the directory with what would be
  written.

## Or an executable

```sh
starci compile --target binary -o ci
./ci            # in a checkout; ./ci path/to/checkout for another
```

writes a copy of starci with the script appended, which does what
`starci run` does and nothing else. Anything after the checkout is a `run`
flag (`-v`, `--event`, `--sha`, `--slots`, `--bare`, `--bare-image`).

By default the script travels and is evaluated when the executable runs, so
advanced scripts and scripts that read `ctx` work as they do under
`starci run`. With `--freeze` the plan is evaluated now and travels instead:
no script is evaluated at run time, advanced scripts are refused, and anything
decided by `ctx` is fixed at what a push about which nothing is known gives,
as in the CircleCI output.

What is inside is starci and the script. Tools are still downloaded on first
use, by checksum, images are pulled by Docker, and the repository is the
checkout it is run in, so it needs Docker and nothing else, and not even Docker
if every step is [bare](bare.md). `--base` names the
starci executable to copy, which is how to build for another OS or
architecture. On macOS the appended copy no longer passes `codesign -v`, though
it runs; a file that was downloaded may need its quarantine flag cleared.

## Or run starci inside the CI tool

The other way is a workflow whose one job calls `starci run`. The CI
tool is then only the trigger. starci reads why the run started from the
tool's own environment (GitHub, Forgejo and Gitea Actions, and CircleCI are
recognised) and the script is told exactly what it would be told had starci
received the event: the tag, the pull request and whether it is from a fork,
the schedule that fired, the files a push changed. Nothing is approximated,
and advanced scripts work.

The job runs every step on the runner: steps in an image with the runner's
Docker, [bare steps](bare.md) on the runner itself. A bare step from a fork's
pull request runs only where starci can tell the runner is thrown away after
the job, which on GitHub is a GitHub-hosted runner (`RUNNER_ENVIRONMENT`), and
`--bare` (or `STARCI_BARE`) overrides that either way. `--bare-image` runs
bare steps in a container instead, on a runner that is kept or for a fork.

How starci gets onto the runner is up to the workflow: a release download
pinned by checksum, `go install`, or an executable written by
`--target binary`. So is keeping passes between runs. A runner starts empty,
so without one of these every step runs every time:

- keep `STARCI_STATE` between jobs with the CI tool's cache, for example on
  GitHub:

  ```yaml
  - uses: actions/cache@v4
    with:
      path: ~/.cache/starci
      key: starci-${{ github.run_id }}
      restore-keys: starci-
  ```

- or set `STARCI_STORE` to a bucket, which every machine and every CI tool can
  share (see [Inputs](inputs.md#sharing-results-between-machines)).
