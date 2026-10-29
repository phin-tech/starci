# Build once, and hand the binary to the steps that use it.
#
# A step sees its inputs and nothing else, so what build makes reaches the
# others only because the script says so: build keeps out/app, and each later
# step is handed it at a path of its own. artifact(build, "out/app") stands for
# what build will keep, and makes the step need build.
#
# The binary is kept by the hash of its content, and the steps handed it are
# keyed on that hash: change a comment, build runs again and makes the same
# binary, and the smoke tests are not run again.

GO = "golang:1.23-bookworm"

def ci(ctx):
    build = step("build", image = GO, run = "CGO_ENABLED=0 go build -o out/app ./cmd/app",
                 inputs = ["go.mod", "go.sum", "cmd", "internal"], keep = ["out/app"])
    app = artifact(build, "out/app")
    return [
        build,
        # The binary alone, in an image without Go.
        step("smoke debian", image = "debian:12-slim", run = "./app --version",
             inputs = ["README.md"], artifacts = {"app": app}),
        step("smoke alpine", image = "alpine:3", run = "./app --version",
             inputs = ["README.md"], artifacts = {"app": app}),
        # The binary beside the tests that drive it from outside.
        step("end to end", image = "debian:12-slim", run = "APP=./bin/app ./e2e/run.sh",
             inputs = ["e2e"], artifacts = {"bin/app": app}),
    ]
