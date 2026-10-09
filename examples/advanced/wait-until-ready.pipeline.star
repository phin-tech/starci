# Start something, wait for it to be ready, then test against it.
#
# The wait is a loop of short steps rather than one long one. Each attempt is
# visible on its own, and the script decides how patient to be.

IMAGE = "alpine:3"
TRIES = 10

def op(name, command):
    return step(name, image = IMAGE, run = command, cache = False)

def pipeline(ctx):
    run(op("create preview", "./scripts/preview.sh create " + ctx.sha))

    ready = False
    for attempt in range(TRIES):
        probe = run(op("probe {}".format(attempt + 1), "sleep 15; ./scripts/preview.sh healthy " + ctx.sha), allow_failure = True)
        if probe.ok:
            ready = True
            break

    if ready:
        tests = run(op("end to end", "./scripts/e2e.sh " + ctx.sha), allow_failure = True)
    # Whatever happened, do not leave the preview running.
    run(op("destroy preview", "./scripts/preview.sh destroy " + ctx.sha))

    if not ready:
        fail("the preview never became healthy after {} tries".format(TRIES))
    if not tests.ok:
        fail("end to end tests failed against the preview")
