# Where starci is not running: compiling to a workflow

A basic script can be written out as a GitHub Actions workflow, which Forgejo
and Gitea Actions read too, or as a CircleCI configuration:

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
- A step that keeps or is handed [artifacts](scripts.md#handing-files-on) is
  refused for now; an executable carries them.
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
has been run by hand. Neither has been run on the service it is for.

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
