# Tests that need a database get one of their own.
#
# A service is a container started beside a step and stopped with it. The step
# reaches it by its name, here `postgres`. It belongs to the step: the two
# test steps below run at the same time, each against its own empty database,
# so neither can see what the other wrote.

postgres = service(
    "postgres",
    image = "postgres:17",
    env = {"POSTGRES_USER": "app", "POSTGRES_PASSWORD": "app", "POSTGRES_DB": "app"},
    # The step starts once this succeeds inside the service's container. It
    # asks over TCP: while the image is setting a database up it runs a server
    # that answers only on its socket, and that one is not the one to wait for.
    ready = "pg_isready -h localhost -U app",
)

def tests(name, path):
    return step(
        name,
        image = "python:3.13-slim",
        run = "pip install -q -e '.[test]' && pytest -q " + path,
        inputs = ["src", "pyproject.toml", path, "tests/conftest.py"],
        env = {"DATABASE_URL": "postgresql://app:app@postgres:5432/app"},
        services = [postgres],
    )

def ci(ctx):
    return [
        step("lint", image = "python:3.13-slim", run = "pip install -q ruff && ruff check .", inputs = ["src", "tests", "pyproject.toml"]),
        tests("api tests", "tests/api"),
        tests("storage tests", "tests/storage"),
    ]
