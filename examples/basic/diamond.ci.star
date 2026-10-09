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

IMAGE = "node:22-bookworm-slim"

def ci(ctx):
    build = step("build", image = IMAGE, run = "npm ci && npm run build")
    lint = step("lint", image = IMAGE, run = "npm ci && npm run lint", needs = [build])
    test = step("test", image = IMAGE, run = "npm ci && npm test", needs = [build])
    package = step("package", image = IMAGE, run = "npm ci && npm pack", needs = [lint, test])
    return [build, lint, test, package]
