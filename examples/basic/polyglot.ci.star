# One repository, several languages: run each language's checks only when
# files of that language changed.
#
# The rules are a table. Adding a language is adding a row.

RULES = [
    # (name, file endings, image, command)
    ("python", (".py", "pyproject.toml", "uv.lock"), "python:3.13-slim", "pip install -q ruff && ruff check ."),
    ("go", (".go", "go.mod", "go.sum"), "golang:1.26", "go vet ./... && go test ./..."),
    ("typescript", (".ts", ".tsx", "package.json"), "node:22-bookworm-slim", "npm ci && npx tsc --noEmit"),
    ("shell", (".sh",), "koalaman/shellcheck-alpine:stable", "shellcheck $(find . -name '*.sh')"),
    ("docker", ("Dockerfile",), "hadolint/hadolint:latest-debian", "hadolint $(find . -name Dockerfile)"),
]

def ci(ctx):
    steps = []
    for name, endings, image, command in RULES:
        # With no list of changes to go on, check everything.
        if not ctx.changed_files or any([f.endswith(endings) for f in ctx.changed_files]):
            steps.append(step(name, image = image, run = command))
    return steps
