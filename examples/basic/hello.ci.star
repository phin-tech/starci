# The smallest script: one step, on every commit.

def ci(ctx):
    return [step("hello", image = "alpine:3", run = "echo hello from " + ctx.sha[:8])]
