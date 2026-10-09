# One script for every reason a run starts.
#
# ctx.event says why: "push", "tag", "pull_request", "schedule" or "manual".
# The script answers with the steps for that reason. There is no separate
# list of triggers to keep in step with it; the one thing that has to be
# declared is a schedule, because nothing causes one.

schedule("nightly", cron = "0 3 * * *")

IMAGE = "node:22-bookworm-slim"

def node(name, command, **kwargs):
    return step(name, image = IMAGE, run = "npm ci && " + command, **kwargs)

def ci(ctx):
    lint = node("lint", "npm run lint")
    test = node("test", "npm test")
    checks = [lint, test]

    # The slow suite, and an audit against a database that changes daily:
    # once a night rather than on every push.
    if ctx.event == "schedule" and ctx.schedule == "nightly":
        return checks + [
            node("end to end", "npm run test:e2e", needs = checks),
            node("audit", "npm audit --audit-level=high", cache = False),
        ]

    # A tag is a release.
    if ctx.event == "tag":
        return checks + [node("publish", "npm publish", needs = checks, cache = False)]

    # A pull request gets a preview to look at. Not from a fork: that would
    # be running a stranger's code with this repository's credentials.
    if ctx.event == "pull_request" and not ctx.pr.fork:
        return checks + [node("preview", "npm run deploy:preview", needs = checks, cache = False)]

    # Pushes to main deploy to staging.
    if ctx.event == "push" and ctx.ref == "main":
        return checks + [node("deploy staging", "npm run deploy:staging", needs = checks, cache = False)]

    return checks
