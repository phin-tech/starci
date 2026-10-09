# A matrix is a loop. Here: every supported Python against two databases, with
# the combinations that make no sense left out by an ordinary `if`.

PYTHONS = ["3.11", "3.12", "3.13"]
DATABASES = ["sqlite", "postgres"]

def ci(ctx):
    steps = []
    for python in PYTHONS:
        for db in DATABASES:
            # Postgres support was added for 3.12; nothing to test before it.
            if db == "postgres" and python == "3.11":
                continue
            steps.append(step(
                "py{} {}".format(python, db),
                image = "python:{}-slim".format(python),
                run = "pip install -e '.[test]' && pytest -q",
                env = {"TEST_DATABASE": db},
                inputs = ["src", "tests", "pyproject.toml"],
            ))
    return steps
