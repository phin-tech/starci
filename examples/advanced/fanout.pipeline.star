# Fan out over something a step finds out.
#
# How many test steps there are is not written here. One step lists the test
# files as they are at this commit, and the script starts a step for each.
# Add a test file and it gets its own step, with no change to this script.

PY = "python:3.13-slim"
SOURCE = ["src", "pyproject.toml"]

def pipeline(ctx):
    listing = run(step("list tests", image = "alpine:3", run = "cd tests && ls test_*.py", inputs = ["tests/test_*.py"]))
    files = listing.output.splitlines()

    parallel([
        step(
            "pytest " + f,
            image = PY,
            run = "pip install -q -e . && pytest -q tests/" + f,
            # Each step is given the source and its one test file, so editing
            # one test file reruns one step.
            inputs = SOURCE + ["tests/" + f, "tests/conftest.py"],
        )
        for f in files
    ])
