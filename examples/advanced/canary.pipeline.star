# Roll a release out in stages, checking each before widening it.
#
# A loop over stages, stopping at the first one that looks wrong. The stages
# are data: adding one is adding a line.

STAGES = [("canary", "1"), ("early", "10"), ("half", "50"), ("everyone", "100")]

def op(name, command):
    return step(name, image = "alpine:3", run = command, cache = False)

def pipeline(ctx):
    if ctx.ref != "main":
        return
    run(op("build", "./scripts/build.sh " + ctx.sha))

    for name, percent in STAGES:
        run(op("release to " + name, "./scripts/release.sh --percent " + percent))
        health = run(op("check " + name, "./scripts/error-rate.sh --below 0.5"), allow_failure = True)
        if not health.ok:
            run(op("withdraw", "./scripts/release.sh --percent 0"))
            fail("error rate too high at {}% ({}); withdrawn".format(percent, name))
