# Run each package's tests, read the coverage each reports, and hold the
# commit to a floor.
#
# Each test step prints its coverage as its last line. The script reads the
# numbers back, so the rule ("nothing under 70, 80 overall") lives in one
# place and can be as particular as it needs to be.

PACKAGES = ["core", "api", "cli"]
EACH_AT_LEAST = 70
OVERALL_AT_LEAST = 80

def pipeline(ctx):
    results = parallel([
        step(
            "test " + p,
            image = "python:3.13-slim",
            run = "pip install -q -e '.[test]' && pytest -q --cov=src/{0} tests/{0} && coverage report --format=total".format(p),
            inputs = ["src/" + p, "tests/" + p, "pyproject.toml"],
        )
        for p in PACKAGES
    ])

    coverage = {r.name.removeprefix("test "): int(r.output.strip().splitlines()[-1]) for r in results}
    low = ["{} at {}%".format(p, c) for p, c in coverage.items() if c < EACH_AT_LEAST]
    total = 0
    for c in coverage.values():
        total += c
    overall = total // len(coverage)

    if low:
        fail("coverage below {}%: {}".format(EACH_AT_LEAST, ", ".join(low)))
    if overall < OVERALL_AT_LEAST:
        fail("overall coverage is {}%, below {}%".format(overall, OVERALL_AT_LEAST))
