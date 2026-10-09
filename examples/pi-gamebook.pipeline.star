# pi-gamebook's CI in advanced mode: the script runs steps itself and reacts
# to what they return. Compare pi-gamebook.ci.star, which declares the same
# checks as a fixed plan.

BUN = "oven/bun:1"
SOURCES = ["src", "test", "rules", "index.ts", "omp.ts", "package.json", "bun.lock", "tsconfig.json"]

def bun(name, command):
    return step(
        name,
        image = BUN,
        run = "bun install --frozen-lockfile >/dev/null && " + command,
        inputs = SOURCES,
        caches = {"bun": "/root/.bun/install/cache"},
    )

def pipeline(ctx):
    run(bun("check", "bun run check"))

    # How many test steps there are is read from the repository as it is at
    # this commit, not written down here.
    listing = run(step("list tests", image = "alpine:3", run = "ls test", inputs = ["test"]))
    files = [f for f in listing.output.splitlines() if f.endswith(".test.ts")]

    results = parallel([bun("test " + f, "bun test test/" + f) for f in files], allow_failure = True)

    # One more try for anything that failed, and only for that.
    failed = [r.name for r in results if not r.ok]
    if failed:
        results = parallel([bun(name, "bun test test/" + name.removeprefix("test ")) for name in failed], allow_failure = True)
        failed = [r.name for r in results if not r.ok]
    if failed:
        fail("failed twice: " + ", ".join(failed))
