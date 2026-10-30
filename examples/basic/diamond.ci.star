# Steps that depend on each other. `needs` takes the step itself or its name.
#
#            build
#           /     \
#        lint     test
#           \     /
#           package
#
# lint and test start together once build has passed. If either fails,
# package is skipped rather than run against something broken.

# Node is a pinned tool, so the steps name no image: they run on the machine
# itself, and compiled for a CI tool they run on the runner without Docker.
node = tool(
    "node",
    url = "https://nodejs.org/dist/v22.23.4/node-v22.23.4-{os}-{arch}.tar.gz",
    sha256 = {
        "amd64": "b8492284c76a54baf58d61668dd0e41ee0fc98adabf9503d30f05d08f55b4406",
        "arm64": "f7a1e486dbc8b286847815b477d4fb5e9fb963ed4489c3e1588853d685f589f9",
        "darwin/amd64": "efcc2b0db6f94bf53eddb038fe2b370277a042bd29e4f19b8f8e23598795b819",
        "darwin/arm64": "c9e1dbc748b33ed2d14f42445191ecaf14c56259c7a9a26d0042c49ec2b66428",
    },
    arch_names = {"amd64": "x64"},
    bin = ["node-v22.23.4-{os}-{arch}/bin"],
)

def ci(ctx):
    build = step("build", tools = [node], run = "npm ci && npm run build")
    lint = step("lint", tools = [node], run = "npm ci && npm run lint", needs = [build])
    test = step("test", tools = [node], run = "npm ci && npm test", needs = [build])
    package = step("package", tools = [node], run = "npm ci && npm pack", needs = [lint, test])
    return [build, lint, test, package]
