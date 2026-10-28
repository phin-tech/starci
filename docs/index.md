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

There are thirty-seven worked examples in [`examples/`](examples/README.md): twenty-five basic and twelve advanced.

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
| `starci workers`, `shun`, `unshun` | See the workers, and take one out of rotation. |
