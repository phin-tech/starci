# Build once, then decide from the binary what to test.
#
# build keeps out/; its result carries what it kept, and the script hands
# that on to every step after it. The binary is asked, in an image without Go,
# which features it was built with, and only those are tested: the steps are
# chosen by what was built, which a fixed plan could not know.

GO = "golang:1.23-bookworm"

def pipeline(ctx):
    build = run(step("build", image = GO, run = "make build",
                     inputs = ["go.mod", "go.sum", "cmd", "internal", "Makefile"], keep = ["out"]))
    out = build.artifacts["out"]

    features = run(step("features", image = "debian:12-slim", run = "./bin/app --features",
                        inputs = ["README.md"], artifacts = {"bin": out})).output.split()

    parallel([
        step("test " + f, image = "debian:12-slim", run = "./e2e/run.sh " + f,
             inputs = ["e2e"], artifacts = {"bin": out})
        for f in features
    ])
