# A monorepo: test only the packages a commit touched.
#
# Each package's step is given that package's directory and the shared library
# as inputs, so a result is also reused whenever those are unchanged, even if
# the commit touched something else entirely.

PACKAGES = ["api", "web", "worker"]
SHARED = ["libs/common", "package.json", "package-lock.json"]

def test(package):
    return step(
        "test " + package,
        image = "node:22-bookworm-slim",
        run = "npm ci && npm test --workspace packages/" + package,
        inputs = ["packages/" + package] + SHARED,
        caches = {"npm": "/root/.npm"},
    )

def ci(ctx):
    touched = [f.split("/")[1] for f in ctx.changed_files if f.startswith("packages/") and f.count("/") >= 2]

    # A change to something every package depends on means all of them.
    shared_changed = any([f.startswith("libs/") or f in SHARED for f in ctx.changed_files])
    if shared_changed or not ctx.changed_files:
        touched = PACKAGES

    return [test(p) for p in PACKAGES if p in touched]
