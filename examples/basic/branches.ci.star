# Different work for different branches.
#
# Everything is checked everywhere. Only main publishes, and a publish is
# never reused from an earlier run: it has an effect outside its inputs.
# It is given the registry's token by name. The value is on the worker.

IMAGE = "node:22-bookworm-slim"

def ci(ctx):
    test = step("test", image = IMAGE, run = "npm ci && npm test")
    steps = [test]

    if ctx.ref == "main":
        steps.append(step(
            "publish",
            image = IMAGE,
            run = "npm ci && npm publish",
            needs = [test],
            cache = False,
            secrets = ["NPM_TOKEN"],
        ))
    elif ctx.ref.startswith("release/"):
        steps.append(step("changelog", image = IMAGE, run = "npm run check-changelog", needs = [test]))

    return steps
