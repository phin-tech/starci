# Steps without an image

A step that names no image is a **bare step**. It runs as the machine's own
processes, in whatever the machine provides: macOS with Xcode on a Mac, your
laptop, the runner of a CI job. It needs no Docker.

```python
def ci(ctx):
    return [
        step("vet", run = ["go", "vet", "./..."], caches = {"mod": "GOMODCACHE"}),
        step("app", run = "xcodebuild -scheme App build", on = {"os": "darwin"}),
        step("lint", run = "flox activate -- make lint"),
    ]
```

A step that wants an environment manager (Flox, Nix, mise) runs it as part of
its command; starci knows nothing of them.

## What it is given

| | |
|---|---|
| Its workspace | The step's inputs and nothing else, as for any step. It is the working directory, and `$STARCI_WORKSPACE`. |
| `PATH` | Its tools, then the machine's own `PATH`. |
| Its environment | `HOME` and `TMPDIR` of its own, `LANG=C.UTF-8`, and its `env`. Nothing else of the machine's environment, which is where a machine keeps its tokens. |
| `caches` | A directory kept between runs on the machine, set in the environment variable it names: `caches = {"mod": "GOMODCACHE"}`. |
| `tools` | Downloaded for the machine's own system. A tool used on macOS has to give a checksum for `darwin/arm64` or `darwin/amd64`; `amd64` and `arm64` alone mean Linux. `{os}` in its `url` is replaced with `linux` or `darwin`. |

`run` may be a list, `run = ["go", "test", "./..."]`, which starts the program
without a shell. That works in an image too.

## When a pass is reused

Not unless the script says so. A bare step ran against whatever the machine
had, which starci cannot know, so it runs every time. With `cache = True` a
pass is reused when the command, its `env`, its inputs, its tools and the
machine's system (`darwin/arm64`) are the same; what is installed on the
machine is not in that, and is the script's to answer for. A step that needs
to say exactly what it runs against wants an image.

## What it cannot have

- `packages`: there is no image to install them into.
- `services`: not yet.
- A container around it. The workspace holds only the step's inputs, but
  nothing stops the step reading elsewhere on the machine, as the machine's
  user.

`network = False` is kept to: on macOS with `sandbox-exec`, which allows
local (Unix) sockets and nothing else; on Linux in a network namespace of its
own, which needs unprivileged user namespaces and has not even loopback.
Elsewhere the step fails rather than run with the network. A compiled workflow
cannot keep a bare step off the network, so such a step is not compiled.

When the step's command ends, whatever it left running ends with it, and so
does everything it started when the run is cancelled. If starci itself is
killed partway, even by `kill -9`, a watchdog it started beside the step
notices and stops the step's processes. A process that leaves the step's
process group (with `setsid`, as some daemons do) escapes all of this.

## Running several at once

Bare steps run side by side like any others (`--slots`, `parallel()`), each
with its own workspace, `HOME`, `TMPDIR` and process group. What they share is
the machine:

- **Ports.** Two bare steps that each start a server on `:8080` collide. A
  step in an image has a network of its own; a bare step does not.
- **Caches.** Two steps naming the same cache use one directory at once. Go's
  caches are made for that; not every tool's is.
- **Anything else of the machine's**: a fixed path under `/tmp`, a simulator,
  a device.

starci does not guard against these. Run fewer at once, order such steps with
`needs`, or write them not to collide.

## In a container instead

`starci run --bare-image IMAGE` (or `STARCI_BARE_IMAGE`) runs every bare
step in a container of `IMAGE` instead of on the machine, as if the step had
named it: a shortcut for wrapping each one in `docker run IMAGE`. Steps that
name their own image are unchanged. The step is given what it would be on
the machine, in the container: its inputs in `/workspace`
(`$STARCI_WORKSPACE`), its `env`, and each cache as a directory the variable
it names points at. Being in a container, it has `network = False` and
reuse as an image step has them, and needs no agreement from the machine. A
step that needs the machine itself, `xcodebuild` on a Mac say, should be run
without it.

## In a virtual machine instead

Docker is not the only boundary to put around a step. Run starci *inside* a
virtual machine (a Lima or Colima guest, a Tart VM, a Firecracker microVM, a
cloud instance) and let the machine be the isolation: the steps are bare,
nothing needs Docker, and a step that goes wrong damages a VM that is thrown
away, not your laptop. A compiled Go program, an `xcodebuild`, anything the
VM can run, is a bare step with its toolchain pinned as a [tool](tools.md).

starci does not start the VM or put the checkout in it. Do that however you
already do, and tell starci the machine is disposable:

```sh
STARCI_BARE=true starci run /path/to/checkout
```

`STARCI_BARE=true` is what lets a pull request from a fork run bare, which
otherwise only a GitHub-hosted runner does. Say it only of a machine that is
thrown away after, because it is the only thing between that code and
whatever else the VM holds, such as credentials left in its environment or
mounted from your own machine.

Steps that name an image still need Docker in the VM. This is documented, not
yet tried against a particular VM tool.

## Who agrees to it

A bare step runs as the machine's user with no container around it, so the
machine has to agree. `starci run` does, it being your machine and your
checkout, except for a pull request from a fork: that is someone else's code,
and runs bare only on a machine thrown away after the job, which on GitHub is
a GitHub-hosted runner (`RUNNER_ENVIRONMENT`).

`--bare` or `STARCI_BARE=true` lets bare steps run, and `--bare=false` or
`STARCI_BARE=false` refuses them, whatever the rest of this says. The flag
wins over the variable.

`on = {"os": "darwin"}` and `on = {"arch": "arm64"}` say what kind of machine
a step needs. The machine checks them itself, so a step that needs another
kind fails saying so.
