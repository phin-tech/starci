# starci

> Development happens at [git.phin.tech/sam/starci](https://git.phin.tech/sam/starci). The copy on GitHub is a
> mirror that serves the documentation site: it takes no pull requests or issues.

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

There are thirty-nine worked examples in [`examples/`](examples/README.md), in both modes.

## Documentation

The documentation is in [`docs/`](docs/index.md), and is built into a site with
`dev/docs.sh` (see [Development](docs/development.md)).

| | |
|---|---|
| [Overview and status](docs/index.md) | What works and what does not. |
| [Getting started](docs/getting-started.md) | Try it against a checkout. |
| [Compiling to a workflow](docs/compiling.md) | Write a basic script out for GitHub Actions or CircleCI, or run starci inside one. |
| [Carrying a run out from another tool](docs/driving.md) | `starci next` and `starci step`, for Temporal, a CI job or a shell loop. |
| [What a script can do](docs/scripts.md) | The language, and basic versus advanced mode. |
| [Tools and packages](docs/tools.md) | Pinned downloads and system packages for a step. |
| [Steps without an image](docs/bare.md) | Bare steps, on the machine itself. |
| [Inputs, networks and services](docs/inputs.md) | What a step sees, skipping, and databases beside a step. |
| [How it fits together](docs/architecture.md) | Engines, asking what is next, and traces. |
| [Examples](examples/README.md) | Thirty-nine scripts, and what the basic ones compile to. |
