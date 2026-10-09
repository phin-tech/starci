# Cheap checks first; expensive ones only if they pass.
#
# Layers by cost, each a batch run together. A formatting mistake is reported
# in seconds, and no machine spends ten minutes on an integration suite for a
# commit that would have been sent back anyway.

IMAGE = "python:3.13-slim"
SETUP = "pip install -q -e '.[dev]' && "

def py(name, command, **kwargs):
    return step(name, image = IMAGE, run = SETUP + command, **kwargs)

def pipeline(ctx):
    # Seconds.
    parallel([
        py("format", "ruff format --check ."),
        py("lint", "ruff check ."),
    ])
    # A minute.
    parallel([
        py("types", "pyright src"),
        py("unit", "pytest -q tests/unit"),
    ])
    # Ten minutes, and wants a big machine.
    run(py("integration", "pytest -q tests/integration", prefer = {"speed": "fast"}))
