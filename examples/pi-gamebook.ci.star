# pi-gamebook's CI as a plan: the same three things its Forgejo Actions
# workflow does, as two steps that can run at the same time on different
# machines.

BUN = "oven/bun:1"

def ci(ctx):
    # Docs-only commits have nothing to check.
    code = [f for f in ctx.changed_files if not f.endswith(".md")]
    if ctx.changed_files and not code:
        return []

    install = "bun install --frozen-lockfile"
    return [
        step("check", image = BUN, run = install + " && bun run check"),
        step("test", image = BUN, run = install + " && bun test"),
    ]
