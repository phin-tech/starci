# Tools

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
| `url` | The download: a zip, a tar archive, or the program itself. `{arch}` becomes the architecture the step runs on, and `{os}` `linux` or `darwin`. |
| `sha256` | The download's checksum for `amd64` and/or `arm64`, which mean Linux, and for `darwin/arm64` or `darwin/amd64` where a [bare step](bare.md) on a Mac uses it. A platform not listed is not supported. |
| `arch_names` | How the publisher spells an architecture, where it differs. |
| `bin` | Directories inside the archive to put on `PATH`. Default: its top level. |

The checksum is the tool's identity. A machine keeps one copy per checksum,
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

## Packages

For what a tool cannot carry — system libraries, a compiler — a step can ask
for apt packages on top of its image:

```python
step("test", image = "debian:12-slim", packages = ["libpq-dev", "git"], run = "...")
```

A machine builds an image from that base and those packages the first time a
step needs it, and reuses it for every step that asks for the same
combination. The image must be Debian or Ubuntu based.

Unlike a tool, a package is not pinned: it is whatever version apt offered on
the day that machine built the image, and it stays that version there until
the base image or the list changes. A step's result is keyed on the base image
and the list, so two machines that built at different times will share a result
even though their packages may differ slightly. Write `name=version` where
that matters.
