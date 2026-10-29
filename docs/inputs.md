# Inputs and skipped steps

A step sees only its `inputs`, by default the whole repository. Nothing else
is mounted, not even `.git`. An input is a file, a directory (everything
beneath it), or a pattern:

```python
BASE = ["src/**/*.py", "pyproject.toml", "uv.lock"]

step("unit", ..., inputs = BASE + ["tests/test_runs.py", "tests/conftest.py"])
step("gpu",  ..., inputs = BASE + ["tests/test_gpu.py"], on = {"gpu": "true"})
```

In a pattern, `*` stays within one directory and `**` crosses them. Patterns
are matched against the commit when the step runs, so a new file that matches
becomes an input with no change to the script, and one that does not match is
never seen. An input that matches nothing is an error.

`starci validate` finds that error without running anything. It asks, of the
repository the script is in, whether each input selects a file that git
tracks: a file that was never added is not in the commit a machine builds. The
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

## Steps without the network

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
machine before the container starts and are not affected.

## Services

A step that needs a database, or a stand-in for some API, asks for it to be
running beside it:

```python
postgres = service(
    "postgres",
    image = "postgres:17",
    env = {"POSTGRES_PASSWORD": "pw"},
    ready = "pg_isready -h localhost",
)

step("test", image = "python:3.13-slim", run = "pytest -q",
     env = {"DATABASE_URL": "postgresql://postgres:pw@postgres:5432/postgres"},
     services = [postgres])
```

The service is started before the step and removed after it, however the step
ended. The step reaches it by its name, on any port it listens on. It is the
step's own: steps that name the same service each get one, on a network no
other step is on, so tests running side by side cannot see each other's data,
and nothing is left from the run before.

`ready` is a command run inside the service's container, every half second
until it succeeds, and the step starts only then. Without one, an image that
declares a health check is waited on until it is healthy, and any other is
taken to be ready as soon as its container has started, which is before most
servers are listening. A service that is not ready within two minutes, or
that stops, fails the step, with the last of what the service printed.
`ready_within` gives one that is slow to start more seconds than that, up to
an hour. The command is run by `sh`, so an image without a shell cannot have
one. For Postgres ask over TCP, as above:
while the image is setting a database up it runs a server that answers only
on its socket, and `pg_isready` alone would be satisfied by that one.

`args` replaces the image's command, for a server that has to be told what to
serve: `service("store", image = "minio/minio", args = ["server", "/data"])`.

`files` puts paths from the repository inside the service's container,
read-only, as repository path to path in the container:

```python
service("postgres", image = "postgres:17", env = {"POSTGRES_PASSWORD": "pw"},
        ready = "pg_isready -h localhost",
        files = {"db/roles.sql": "/docker-entrypoint-initdb.d/10-roles.sql"})
```

Each is one file or one directory, not a pattern. They are the service's
alone: the step sees them only if they are also among its `inputs`. A file
that is not in the commit fails the step, and `starci validate` looks for
them as it does for inputs.

A service is part of what a pass means. A step's result is keyed on each
service's name, image, `env`, `args`, `ready` and the contents of its `files`,
and the image is identified by its content like the step's own, so a new
`postgres:17` or an edited script runs the step again. A step with `network = False` still reaches its services, and they
reach nothing else either: the step's container has a network interface, but
no route out.

Each step with services takes one of the machine's Docker networks for as
long as it runs. Docker has about thirty to give unless its
`default-address-pools` are widened, which bounds how many such steps a
machine can run at once. If starci is killed partway, the services of the
steps it was running are left behind; they carry the label `starci.machine`,
so `docker rm -f $(docker ps -aq --filter label=starci.machine)` clears them
and `docker network prune --filter label=starci.machine` their networks.

## Sharing results between machines

By default a machine knows only the results of steps it ran itself. Point
several at one store and a step that passed on any of them is skipped on all:

```sh
export STARCI_STORE='s3://starci-results/v1?endpoint=<account>.r2.cloudflarestorage.com'
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
```

Any S3-compatible bucket works; it has been run against Cloudflare R2. Each
machine still keeps its own copy and asks the shared store only for what it
does not have. The store is checked when a machine or a local run starts, so a
wrong address or credential is an error then, not steps quietly running again.
If it becomes unreachable later, steps run and their results are kept locally.

Results are keyed on the architecture as well as the image, so a pass on an
arm64 laptop is never taken as a pass on an amd64 server.

**Whoever can write to the store can make every machine skip a step**, by
storing a pass it never earned. Give its credentials only to machines you
trust to run steps honestly.

`caches = {"bun": "/root/.bun/install/cache"}` keeps a directory between runs
on a machine. It makes a step that does run faster, and is not part of the key.
A [bare step](bare.md) names the environment variable to set to the directory
instead of a path.
