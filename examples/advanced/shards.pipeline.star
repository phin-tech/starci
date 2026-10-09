# Split a test suite into shards sized by what a step measures.
#
# The first step counts the tests. The script then decides how many shards
# are worth starting: one for a small suite, more as it grows, never more than
# the workers can usefully run at once.

IMAGE = "python:3.13-slim"
PER_SHARD = 200
MOST = 8

def pipeline(ctx):
    counted = run(step(
        "count tests",
        image = IMAGE,
        run = "pip install -q -e . && pytest --collect-only -q | grep -c '::'",
        inputs = ["src", "tests", "pyproject.toml"],
    ))
    total = int(counted.output.strip().splitlines()[-1])
    shards = max(1, min(MOST, (total + PER_SHARD - 1) // PER_SHARD))

    parallel([
        step(
            "tests {}/{}".format(i + 1, shards),
            image = IMAGE,
            run = "pip install -q -e . pytest-split && pytest -q --splits {} --group {}".format(shards, i + 1),
            inputs = ["src", "tests", "pyproject.toml"],
        )
        for i in range(shards)
    ])
