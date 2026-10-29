# Test, then deploy only from main, then check the deployment and undo it if
# the check fails.
#
# The script reads like the runbook it replaces. Nothing after a failed step
# runs unless the script asked to be told about the failure instead.

IMAGE = "alpine:3"

def sh(name, command, **kwargs):
    # Deploying and checking have effects and read the outside world: they
    # must run every time, never be reused from an earlier run.
    return step(name, image = IMAGE, run = command, cache = False, **kwargs)

def pipeline(ctx):
    parallel([
        step("lint", image = IMAGE, run = "./scripts/lint.sh"),
        step("test", image = IMAGE, run = "./scripts/test.sh"),
    ])

    if ctx.ref != "main":
        return

    run(sh("deploy", "./scripts/deploy.sh " + ctx.sha, on = {"site": "office"}))

    smoke = run(sh("smoke test", "./scripts/smoke.sh"), allow_failure = True)
    if not smoke.ok:
        run(sh("roll back", "./scripts/rollback.sh", on = {"site": "office"}))
        fail("deployed " + ctx.sha[:8] + " but the smoke test failed; rolled back")
