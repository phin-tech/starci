# Checks that should be seen but should not block.
#
# The required checks stop the run if they fail. The advisory ones run
# alongside, never stop anything, and are summed up in one line at the end.

IMAGE = "node:22-bookworm-slim"

def check(name, command):
    return step(name, image = IMAGE, run = "npm ci >/dev/null && " + command)

def pipeline(ctx):
    advisory = parallel([
        check("bundle size", "npm run size-limit"),
        check("unused exports", "npm run knip"),
        check("spelling", "npm run cspell"),
    ], allow_failure = True)

    parallel([
        check("types", "npm run typecheck"),
        check("tests", "npm test"),
    ])

    grumbles = [r.name for r in advisory if not r.ok]
    if grumbles:
        run(step("advisory", image = "alpine:3", run = "echo 'worth a look: " + ", ".join(grumbles) + "'", cache = False))
