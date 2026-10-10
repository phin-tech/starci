# Build once, and give what was built to the steps that need it.
#
# `outputs` names what a step leaves behind. Listing the step among another
# step's `inputs` gives that step those files, at the paths they were made
# at, and makes it wait for them: no `needs` is written.
#
# The tests are given `dist/` and never the sources it was built from, so a
# change to a test runs the tests again and not the build. And when a commit
# changes nothing the build reads, the build is skipped and still hands on
# what it made last time.

IMAGE = "node:22-bookworm-slim"

def ci(ctx):
    build = step(
        "build",
        image = IMAGE,
        run = "npm ci && npm run build",
        inputs = ["src", "package.json", "package-lock.json"],
        outputs = ["dist"],
    )
    return [
        build,
        step("unit", image = IMAGE, run = "node --test tests/unit", inputs = ["tests/unit", build]),
        step("browser", image = IMAGE, run = "node --test tests/browser", inputs = ["tests/browser", build]),
        # Given nothing from the repository at all: only what was built.
        step("size", image = IMAGE, run = "du -sk dist", inputs = [build]),
    ]
