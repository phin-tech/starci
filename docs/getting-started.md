# Trying it

Needs Go, Docker and git.

```sh
go build -o starci ./cmd/starci

./starci run ~/src/pi-gamebook --file examples/basic/pi-gamebook.ci.star
```

```
src/pi-gamebook @ e8da515a: 2 step(s)
  check  |  test

  passed   check  2.9s on this machine
  passed   test  2.0s on this machine

PASSED in 2.9s
```

`starci run` builds what is committed, fetched from the checkout itself, with
each step in a container that sees only its inputs, or [bare](bare.md) on the
machine. `-v` prints each step's output as it runs; Ctrl-C stops the steps
and removes their containers. There is no queue and nothing to resume from if
it is stopped. `--bare-image golang:1.26` runs the bare steps in that image
instead.

Inside another CI tool's job it is the same command: the tool triggers it and
shows how it went, and starci reads why the run started from the tool's
environment. See [Compiling](compiling.md#or-run-starci-inside-the-ci-tool).
