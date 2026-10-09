# Scripts are programs: repeated shapes become functions, and a "template" is
# a function that returns steps.

def lint_test_build(name, image, directory, lint, test, build):
    """The usual three steps for one component, chained."""
    common = dict(image = image, inputs = [directory])
    in_dir = "cd {} && ".format(directory)
    lint_step = step(name + " lint", run = in_dir + lint, **common)
    test_step = step(name + " test", run = in_dir + test, **common)
    build_step = step(name + " build", run = in_dir + build, needs = [lint_step, test_step], **common)
    return [lint_step, test_step, build_step]

def ci(ctx):
    components = {
        "ui": lint_test_build("ui", "node:22-bookworm-slim", "ui", "npm ci && npm run lint", "npm ci && npm test", "npm ci && npm run build"),
        "api": lint_test_build("api", "python:3.13-slim", "api", "pip install ruff && ruff check .", "pip install -e . && pytest -q", "pip install build && python -m build"),
    }

    # Keep the components this commit touched; all of them if it is not known.
    wanted = [name for name in components if name in ctx.changed_dirs] or list(components)
    steps = []
    for name in wanted:
        steps += components[name]
    return steps
