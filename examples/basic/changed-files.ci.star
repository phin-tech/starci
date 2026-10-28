# What tj-actions/changed-files and dorny/paths-filter are used for, without
# an action: ask which of the changed files match some globs.
#
#   match_files(files, include = [], exclude = [])   the files that match
#   paths_filter(files, {name: [globs]})             a list of matches per name
#
# Both take a list of paths, here ctx.changed_files. Globs match the whole
# path: * stays inside a directory, ** does not, **/ may match no directory
# at all, {a,b} is either, and a pattern starting with ! takes away what it
# matches from the rest of its list.

FILTERS = {
    "api": ["api/**", "!api/**/*.md"],
    "ui": ["ui/**/*.{ts,tsx,css}", "ui/package*.json"],
    "docs": ["docs/**", "**/*.md"],
    "infra": ["terraform/**", ".github/**"],
}

def ci(ctx):
    # With no list of changes to go on, everything counts as changed.
    changed = ctx.changed_files or ["api/", "ui/", "docs/", "terraform/"]
    hit = paths_filter(changed, FILTERS)
    steps = []

    if hit["api"]:
        steps.append(step("api tests", image = "python:3.13-slim", run = "cd api && pip install -e . && pytest -q", inputs = ["api"]))
    if hit["ui"]:
        steps.append(step("ui tests", image = "node:22-bookworm-slim", run = "cd ui && npm ci && npm test", inputs = ["ui"]))

    # Files, not just whether there were any: check only the Markdown that
    # changed, apart from the generated changelog.
    markdown = match_files(changed, include = ["**/*.md"], exclude = ["CHANGELOG.md"])
    if markdown:
        steps.append(step(
            "markdown lint",
            image = "davidanson/markdownlint-cli2:latest",
            run = "markdownlint-cli2 " + " ".join(markdown),
            inputs = markdown,
        ))

    if hit["infra"]:
        steps.append(step("terraform fmt", image = "hashicorp/terraform:1.9", run = "terraform fmt -check -recursive terraform", inputs = ["terraform"]))
    return steps
