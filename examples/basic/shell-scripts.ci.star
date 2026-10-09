# Lint the shell scripts in a repository, with two small tools and no image
# built for the purpose. The step sees only the scripts.

shellcheck = tool(
    "shellcheck",
    url = "https://github.com/koalaman/shellcheck/releases/download/v0.11.0/shellcheck-v0.11.0.linux.{arch}.tar.xz",
    sha256 = {
        "amd64": "8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198",
        "arm64": "12b331c1d2db6b9eb13cfca64306b1b157a86eb69db83023e261eaa7e7c14588",
    },
    arch_names = {"amd64": "x86_64", "arm64": "aarch64"},
    bin = ["shellcheck-v0.11.0"],
)

# A single binary rather than an archive: it is saved under the tool's name.
jq = tool(
    "jq",
    url = "https://github.com/jqlang/jq/releases/download/jq-1.8.1/jq-linux-{arch}",
    sha256 = {
        "amd64": "020468de7539ce70ef1bceaf7cde2e8c4f2ca6c3afb84642aabc5c97d9fc2a0d",
        "arm64": "6bc62f25981328edd3cfcfe6fe51b073f2d7e7710d7ef7fcdac28d4e384fc3d4",
    },
)

def ci(ctx):
    return [
        step(
            "shellcheck",
            image = "debian:12-slim",
            tools = [shellcheck],
            run = "shellcheck scripts/*.sh",
            inputs = ["scripts/*.sh"],
        ),
        step(
            "json is valid",
            image = "debian:12-slim",
            tools = [jq],
            run = "for f in config/*.json; do jq empty \"$f\" || exit 1; done",
            inputs = ["config/*.json"],
        ),
    ]
