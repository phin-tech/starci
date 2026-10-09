# pi-gamebook's CI with Bun brought as a tool rather than baked into an image:
# the steps run on a plain Debian image, and Bun is put on their PATH.

# Pinned by checksum, one per architecture. The same archive is downloaded once
# per machine and shared, read-only, by every step that lists it.
bun = tool(
    "bun",
    url = "https://github.com/oven-sh/bun/releases/download/bun-v1.3.14/bun-linux-{arch}.zip",
    sha256 = {
        "amd64": "951ee2aee855f08595aeec6225226a298d3fea83a3dcd6465c09cbccdf7e848f",
        "arm64": "a27ffb63a8310375836e0d6f668ae17fa8d8d18b88c37c821c65331973a19a3b",
    },
    arch_names = {"amd64": "x64", "arm64": "aarch64"},
    bin = ["bun-linux-{arch}"],
)

SOURCES = ["src", "test", "rules", "index.ts", "omp.ts", "package.json", "bun.lock", "tsconfig.json"]

def bun_step(name, command):
    return step(
        name,
        image = "debian:12-slim",
        tools = [bun],
        run = "bun install --frozen-lockfile && " + command,
        inputs = SOURCES,
        caches = {"bun": "/root/.bun/install/cache"},
    )

def ci(ctx):
    return [
        bun_step("check", "bun run check"),
        bun_step("test", "bun test"),
    ]
