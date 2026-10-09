# Let a step decide what the rest of the run is.
#
# The first step inspects the commit and prints one word per kind of work it
# calls for. Which heavy checks run is then a question for the repository's
# own tooling, answered at this commit, not a list kept here by hand.

IMAGE = "node:22-bookworm-slim"

# What each word leads to.
CHECKS = {
    "migrations": ("migration dry run", "npm run migrate:check"),
    "api": ("contract tests", "npm run test:contract"),
    "ui": ("visual regression", "npm run test:visual"),
    "deps": ("licence audit", "npm run licences"),
}

def pipeline(ctx):
    run(step("unit", image = IMAGE, run = "npm ci && npm test"))

    asked = run(step("classify", image = IMAGE, run = "node scripts/classify-change.js", cache = False))
    wanted = [w for w in asked.output.split() if w in CHECKS]

    parallel([
        step(CHECKS[w][0], image = IMAGE, run = "npm ci && " + CHECKS[w][1])
        for w in wanted
    ])
