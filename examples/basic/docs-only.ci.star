# Do nothing for a commit that only touches documentation.
#
# The plan is computed from what the commit changed, so "nothing to do" is an
# empty list, and the commit is marked as passing without a container starting.

DOCS = (".md", ".txt", ".rst")

def is_docs(path):
    return path.endswith(DOCS) or path.startswith("docs/")

def ci(ctx):
    if ctx.changed_files and all([is_docs(f) for f in ctx.changed_files]):
        return []
    return [step("test", image = "python:3.13-slim", run = "pip install -e . && pytest -q")]
