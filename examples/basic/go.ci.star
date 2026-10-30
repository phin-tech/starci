# A Go module: vet and test once, then build for each platform it ships on.
# The toolchain is a pinned tool, so the steps are bare: they run on the machine
# itself with no image, and compiled for a CI tool they run on the runner
# without Docker. The checksums are the only thing the Go version is.

golang = tool(
    "go",
    url = "https://go.dev/dl/go1.26.5.{os}-{arch}.tar.gz",
    sha256 = {
        "amd64": "5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053",
        "arm64": "fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49",
        "darwin/amd64": "6231d8d3b8f5552ec6cbf6d685bdd5482e1e703214b120e89b3bf0d7bf1ef725",
        "darwin/arm64": "efb87ff28af9a188d0536ef5d42e63dd52ba8263cd7344a993cc48dd11dedb6a",
    },
    bin = ["go/bin"],
)

SOURCES = ["**/*.go", "go.mod", "go.sum"]
PLATFORMS = [("linux", "amd64"), ("linux", "arm64"), ("darwin", "arm64")]

def go(name, command, env = {}, needs = []):
    return step(
        name,
        tools = [golang],
        run = command,
        env = dict(env, CGO_ENABLED = "0"),
        inputs = SOURCES,
        needs = needs,
        # Go keeps its own record of which packages' tests are still valid.
        # Keeping its caches between runs lets it skip those inside a step
        # that does have to run.
        caches = {"go-mod": "GOMODCACHE", "go-build": "GOCACHE"},
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
