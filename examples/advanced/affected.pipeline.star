# Ask the build tool what a commit affects, and test exactly that.
#
# Working out which packages depend on a changed file is the build tool's job,
# and it knows the import graph. One step asks it; the script acts on the
# answer. Nothing here has to be kept in step with the code.

GO = "golang:1.26"

def pipeline(ctx):
    if not ctx.changed_files:
        run(step("test everything", image = GO, run = "go test ./..."))
        return

    asked = run(step(
        "affected packages",
        image = GO,
        # Every package that imports, directly or not, a package with a changed file.
        run = "./scripts/affected.sh " + " ".join(ctx.changed_files),
        # The answer depends on which files changed, which is not an input.
        cache = False,
    ))
    packages = [p for p in asked.output.splitlines() if p]
    if not packages:
        return

    # A handful of packages per step: one step each would spend longer
    # starting containers than testing.
    batch = 5
    parallel([
        step("test batch {}".format(i // batch + 1), image = GO, run = "go test " + " ".join(packages[i:i + batch]))
        for i in range(0, len(packages), batch)
    ])
