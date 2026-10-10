# Examples

Every script here is evaluated by the tests (`internal/plan/examples_test.go`),
which also check that each does what its comments say. Try one against any
checkout:

```sh
starci plan --file examples/basic/monorepo.ci.star --changed packages/api/src/main.ts
starci run ~/src/some-repo --local --file examples/basic/hello.ci.star
```

Most refer to scripts and files a real repository would have (`npm test`,
`./scripts/deploy.sh`), so they plan anywhere but only run where those exist.
The three `pi-gamebook` ones run as they are against that repository.

## Basic: `ci(ctx)` returns the steps

| | |
|---|---|
| [hello](basic/hello.ci.star) | The smallest script. |
| [diamond](basic/diamond.ci.star) | Steps that depend on each other. |
| [docs-only](basic/docs-only.ci.star) | Do nothing for a documentation commit. |
| [events](basic/events.ci.star) | One script for pushes, tags, pull requests, a nightly schedule and manual runs. |
| [branches](basic/branches.ci.star) | Publish from main only; `cache = False` for steps with effects. |
| [matrix](basic/matrix.ci.star) | A matrix is a loop, with exclusions as an `if`. |
| [monorepo](basic/monorepo.ci.star) | Test only the packages a commit touched. |
| [polyglot](basic/polyglot.ci.star) | Per-language checks, chosen by which files changed. |
| [helpers](basic/helpers.ci.star) | Functions that return groups of steps. |
| [python-uv](basic/python-uv.ci.star) | A tool (uv), apt packages, and input patterns. |
| [services](basic/services.ci.star) | A database beside each test step, its own and ready before the step starts. |
| [go](basic/go.ci.star) | The Go toolchain as a tool; builds for several platforms. |
| [rust](basic/rust.ci.star) | Cargo, with its caches kept between runs. |
| [terraform](basic/terraform.ci.star) | Validate each stack that changed. |
| [shell-scripts](basic/shell-scripts.ci.star) | Two small tools, one an archive and one a single binary. |
| [security](basic/security.ci.star) | Secrets, vulnerable dependencies, Dockerfile lint. |
| [gpu](basic/gpu.ci.star) | Shared base inputs plus per-step test files; a step that needs a GPU worker. |
| [placement](basic/placement.ci.star) | `worker`, `prefer` and `avoid`. |
| [pi-gamebook](basic/pi-gamebook.ci.star) | A real repository's CI. |
| [pi-gamebook-tools](basic/pi-gamebook-tools.ci.star) | The same, on plain Debian with Bun as a tool. |

## Advanced: `pipeline(ctx)` runs steps and reacts

| | |
|---|---|
| [fanout](advanced/fanout.pipeline.star) | One step per test file, found by a step. |
| [shards](advanced/shards.pipeline.star) | Count the tests, then choose how many shards to run. |
| [affected](advanced/affected.pipeline.star) | Ask the build tool what a commit affects; test that. |
| [choose-by-step](advanced/choose-by-step.pipeline.star) | A step decides which heavy checks the commit needs. |
| [first-failure](advanced/first-failure.pipeline.star) | Cheap checks first, expensive ones only if they pass. |
| [retry-flaky](advanced/retry-flaky.pipeline.star) | Retry only what failed, and report what passed only on retry. |
| [tolerated](advanced/tolerated.pipeline.star) | Advisory checks that are reported but never block. |
| [coverage-gate](advanced/coverage-gate.pipeline.star) | Read numbers out of steps and hold the commit to a rule. |
| [deploy-gate](advanced/deploy-gate.pipeline.star) | Deploy from main, smoke test, roll back on failure. |
| [canary](advanced/canary.pipeline.star) | Widen a release stage by stage, withdrawing at the first bad sign. |
| [wait-until-ready](advanced/wait-until-ready.pipeline.star) | Poll until something is up, test it, always clean up. |
| [pi-gamebook](advanced/pi-gamebook.pipeline.star) | A real repository: fan out over its test files, retry failures. |
