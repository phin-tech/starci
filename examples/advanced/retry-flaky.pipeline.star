# Run everything, then give whatever failed two more tries, and only that.
#
# A basic plan can only rerun a whole step by rerunning the whole plan. Here
# the script sees which steps failed and decides what to do about each.

SUITES = ["unit", "api", "browser", "migrations"]
ATTEMPTS = 3

def suite(name):
    # cache = False: a test known to be flaky has no trustworthy stored result.
    return step("test " + name, image = "node:22-bookworm-slim", run = "npm ci && npm run test:" + name, cache = False)

def pipeline(ctx):
    waiting = SUITES
    flaky = []
    for attempt in range(ATTEMPTS):
        results = parallel([suite(name) for name in waiting], allow_failure = True)
        failed = [r.name.removeprefix("test ") for r in results if not r.ok]
        if attempt > 0:
            # Passed on a retry: it works, but it is not reliable. Say so.
            flaky += [name for name in waiting if name not in failed]
        waiting = failed
        if not waiting:
            break

    if waiting:
        fail("still failing after {} attempts: {}".format(ATTEMPTS, ", ".join(waiting)))
    if flaky:
        run(step("note flaky", image = "alpine:3", run = "echo 'passed only on retry: " + ", ".join(flaky) + "'", cache = False))
