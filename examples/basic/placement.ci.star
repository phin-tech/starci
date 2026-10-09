# Saying where steps may run.
#
#   worker  the step runs only on a worker with these labels
#   prefer  it would like such a worker, and takes another if none is free
#   avoid   it never runs on a worker with these labels
#
# Workers advertise labels when they start: STARCI_LABELS=network=home,speed=fast

def ci(ctx):
    return [
        # Anything that runs code from a pull request stays off the home network.
        step("test", image = "alpine:3", run = "./scripts/test.sh", avoid = {"network": "home"}),

        # Heavy, but not picky.
        step("benchmark", image = "alpine:3", run = "./scripts/bench.sh", prefer = {"speed": "fast"}),

        # Needs to reach the staging database, which only one site can.
        step("migrate staging", image = "alpine:3", run = "./scripts/migrate.sh", worker = {"site": "office"}, cache = False),
    ]
