# Saying where steps may run.
#
#   on      the step runs only on a machine with these labels
#   prefer  it would like such a machine, and takes another if none is free
#   avoid   it never runs on a machine with these labels
#
# os and arch mean the same to every engine, and the machine checks them
# itself. Other labels mean something only to an engine that places steps by
# label; the local engine says, before the run, that it ignores them.

def ci(ctx):
    return [
        # Anything that runs code from a pull request stays off the home network.
        step("test", image = "alpine:3", run = "./scripts/test.sh", avoid = {"network": "home"}),

        # Heavy, but not picky.
        step("benchmark", image = "alpine:3", run = "./scripts/bench.sh", prefer = {"speed": "fast"}),

        # Needs to reach the staging database, which only one site can.
        step("migrate staging", image = "alpine:3", run = "./scripts/migrate.sh", on = {"site": "office"}, cache = False),
    ]
