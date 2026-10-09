# A Python project on a plain Debian image, with uv brought as a tool and a
# system library installed with apt. Lint, types and tests are separate steps
# so they run at the same time and are reused separately.

uv = tool(
    "uv",
    url = "https://github.com/astral-sh/uv/releases/download/0.12.1/uv-{arch}-unknown-linux-gnu.tar.gz",
    sha256 = {
        "amd64": "90b2f223fb69d19db49e117da601f64978593417988530aa733d456141b4bcbb",
        "arm64": "769d373e146692c639b5fbaae33b331c297a32e03d30448772051902df52bbf4",
    },
    arch_names = {"amd64": "x86_64", "arm64": "aarch64"},
    bin = ["uv-{arch}-unknown-linux-gnu"],
)

PROJECT = ["pyproject.toml", "uv.lock"]

def py(name, command, inputs):
    return step(
        name,
        image = "debian:12-slim",
        tools = [uv],
        packages = ["ca-certificates", "libpq5"],
        run = "uv sync --frozen && uv run --no-sync " + command,
        inputs = PROJECT + inputs,
        caches = {"uv": "/root/.cache/uv"},
    )

def ci(ctx):
    return [
        py("format", "ruff format --check src tests", ["src/**/*.py", "tests/**/*.py"]),
        py("lint", "ruff check src tests", ["src/**/*.py", "tests/**/*.py"]),
        py("types", "pyright src", ["src/**/*.py"]),
        py("test", "pytest -q", ["src", "tests"]),
    ]
