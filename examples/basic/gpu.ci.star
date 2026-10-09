# Tests that need different machines.
#
# Every test step shares a base set of inputs — the source — and adds the test
# files it actually runs. A step is then reused unless the source or its own
# tests changed, and the GPU tests go only to a worker that has one.

BASE = ["src/**/*.py", "pyproject.toml", "uv.lock", "tests/conftest.py"]

def pytest(name, files, worker = {}, prefer = {}):
    return step(
        name,
        image = "ghcr.io/astral-sh/uv:python3.13-bookworm-slim",
        run = "uv sync --frozen && uv run pytest -q " + " ".join(files),
        inputs = BASE + files,
        worker = worker,
        prefer = prefer,
    )

def ci(ctx):
    return [
        pytest("unit", ["tests/test_runs.py", "tests/test_runs2.py"]),
        pytest("integration", ["tests/integration/**/*.py"], prefer = {"speed": "fast"}),
        # Waits for a GPU worker rather than failing on a machine without one.
        pytest("gpu", ["tests/test_gpu.py"], worker = {"gpu": "true"}),
    ]
