# Wrapping the linting actions: golangci/golangci-lint-action, astral-sh/ruff-action,
# ludeeus/action-shellcheck, rhysd/actionlint. Each is a tool and a command, so
# each wrapper is an image and the flags the action would have passed.
#
# Like the actions, each lints only what it is about: a step's inputs are the
# files of its kind, so a commit that touches none of them skips it.

def golangci_lint(version = "v2.1", args = "", working_directory = "."):
    """golangci/golangci-lint-action."""
    return step(
        "golangci-lint",
        image = "golangci/golangci-lint:{}".format(version),
        run = "cd {} && golangci-lint run {}".format(working_directory, args),
        inputs = [working_directory + "/**/*.go", working_directory + "/go.mod", working_directory + "/go.sum", working_directory + "/.golangci.y*ml"],
        caches = {"golangci-lint": "/root/.cache/golangci-lint", "go-build": "/root/.cache/go-build"},
    )

def ruff(args = "check", src = "."):
    """astral-sh/ruff-action."""
    return step(
        "ruff",
        image = "ghcr.io/astral-sh/ruff:latest",
        run = "ruff {} {}".format(args, src),
        inputs = ["**/*.py", "pyproject.toml", "ruff.toml"],
    )

def shellcheck(severity = "warning", ignore_paths = []):
    """ludeeus/action-shellcheck."""
    skip = " ".join(["-not -path './{}/*'".format(p) for p in ignore_paths])
    return step(
        "shellcheck",
        image = "koalaman/shellcheck-alpine:stable",
        run = "find . -name '*.sh' {} -exec shellcheck --severity={} {{}} +".format(skip, severity),
        inputs = ["**/*.sh"],
    )

def actionlint():
    """rhysd/actionlint, for the workflows this repository still has."""
    return step(
        "actionlint",
        image = "rhysd/actionlint:latest",
        run = "actionlint",
        inputs = [".github/workflows"],
    )

# What each linter is about, for deciding from the changed files which to run.
ABOUT = {
    "golangci-lint": ["**/*.go", "go.mod", "go.sum", ".golangci.y*ml"],
    "ruff": ["**/*.py", "pyproject.toml", "ruff.toml"],
    "shellcheck": ["**/*.sh"],
    "actionlint": [".github/workflows/**"],
}

def ci(ctx):
    linters = {
        "golangci-lint": golangci_lint(args = "--timeout 5m"),
        "ruff": ruff(),
        "shellcheck": shellcheck(ignore_paths = ["vendor"]),
        "actionlint": actionlint(),
    }
    # With no list of changes to go on, run them all.
    if not ctx.changed_files:
        return list(linters.values())
    hit = paths_filter(ctx.changed_files, ABOUT)
    return [linters[name] for name in linters if hit[name]]
