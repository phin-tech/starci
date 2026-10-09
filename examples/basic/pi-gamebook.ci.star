# pi-gamebook's CI as a plan: the same things its Forgejo Actions workflow
# does, as two steps that can run at the same time on different machines.

BUN = "oven/bun:1"

# What the type-checker and the tests read. A commit that changes nothing
# under these paths (the README, the CI config) reuses the last passing result.
SOURCES = ["src", "test", "rules", "index.ts", "omp.ts", "package.json", "bun.lock", "tsconfig.json"]

def bun_step(name, command):
    return step(
        name,
        image = BUN,
        run = "bun install --frozen-lockfile && " + command,
        inputs = SOURCES,
        caches = {"bun": "/root/.bun/install/cache"},
    )

def ci(ctx):
    return [
        bun_step("check", "bun run check"),
        bun_step("test", "bun test"),
    ]
