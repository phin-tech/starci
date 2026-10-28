# The usual use of a changed-files action: build a matrix from it. One job per
# service that changed, each with its own image and command.
#
# In a workflow that takes a job to find the changes, write them to an output,
# and read that back in `strategy.matrix`. Here the list is there already, and
# a matrix is a loop.

SERVICES = {
    "billing": {"path": "services/billing", "image": "golang:1.26", "test": "go test ./..."},
    "search": {"path": "services/search", "image": "python:3.13-slim", "test": "pip install -e . && pytest -q"},
    "web": {"path": "services/web", "image": "node:22-bookworm-slim", "test": "npm ci && npm test"},
}

# Changes to these affect every service.
SHARED = ["libs/**", "proto/**", ".ci.star"]

def ci(ctx):
    files = ctx.changed_files
    everything = not files or match_files(files, include = SHARED)

    # A filter for each service: its own directory, except its docs.
    hit = paths_filter(files, {name: [s["path"] + "/**", "!" + s["path"] + "/**/*.md"] for name, s in SERVICES.items()})

    steps = []
    for name in sorted(SERVICES):
        if everything or hit[name]:
            s = SERVICES[name]
            steps.append(step(
                "test " + name,
                image = s["image"],
                run = "cd {} && {}".format(s["path"], s["test"]),
                inputs = [s["path"]] + SHARED[:2],
            ))
    return steps
