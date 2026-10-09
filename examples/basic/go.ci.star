# A Go module: vet and test once, then build for each platform it ships on.
# The toolchain is a tool, so the steps need nothing but a base image.

golang = tool(
    "go",
    url = "https://go.dev/dl/go1.26.5.linux-{arch}.tar.gz",
    sha256 = {
        "amd64": "5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053",
        "arm64": "fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49",
    },
    bin = ["go/bin"],
)

SOURCES = ["**/*.go", "go.mod", "go.sum"]
PLATFORMS = [("linux", "amd64"), ("linux", "arm64"), ("darwin", "arm64")]

def go(name, command, env = {}, needs = []):
    return step(
        name,
        image = "debian:12-slim",
        tools = [golang],
        packages = ["ca-certificates"],
        run = command,
        env = dict(env, CGO_ENABLED = "0"),
        inputs = SOURCES,
        needs = needs,
        # Go keeps its own record of which packages' tests are still valid.
        # Keeping its caches between runs lets it skip those inside a step
        # that does have to run.
        caches = {"go-mod": "/root/go/pkg/mod", "go-build": "/root/.cache/go-build"},
    )

def ci(ctx):
    vet = go("vet", "go vet ./...")
    test = go("test", "go test ./...")
    builds = [
        go(
            "build {}/{}".format(goos, goarch),
            "go build -o /dev/null ./...",
            env = {"GOOS": goos, "GOARCH": goarch},
            needs = [vet, test],
        )
        for goos, goarch in PLATFORMS
    ]
    return [vet, test] + builds
