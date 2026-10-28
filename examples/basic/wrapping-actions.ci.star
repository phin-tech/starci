# Wrapping a GitHub action: a function that returns the step it stands for.
# Most actions are doing one of a few things, and each has a plainer form here.
#
#   actions/checkout               not needed: a step is given its inputs
#   actions/setup-node, -python    a different image, picked per step
#   actions/cache                  caches = {name: path}
#   actions/upload-artifact        no equivalent: a step's output is its log
#   aquasecurity/trivy-action      an image that is the tool, and a command
#   hadolint/hadolint-action       the same
#   docker/build-push-action       a step in the docker image, see below
#
# Wrapping one means writing its `with:` inputs as the function's arguments,
# so that a repository's scripts read like the workflow they replace:
#
#   setup_node(version = "22", run = "npm test")      ~  uses: actions/setup-node
#                                                        with: node-version: 22

def setup_node(name, run, version = "22", cache_dir = "~/.npm", inputs = ["."], needs = []):
    """actions/setup-node with cache: npm."""
    return step(
        name,
        image = "node:{}-bookworm-slim".format(version),
        run = run,
        inputs = inputs,
        needs = needs,
        caches = {"npm-" + version: cache_dir.replace("~", "/root")},
    )

def setup_python(name, run, version = "3.13", needs = []):
    """actions/setup-python with cache: pip."""
    return step(
        name,
        image = "python:{}-slim".format(version),
        run = run,
        needs = needs,
        caches = {"pip-" + version.replace(".", "-"): "/root/.cache/pip"},
    )

def trivy(name = "trivy", target = ".", severity = "HIGH,CRITICAL", scanners = "vuln,secret"):
    """aquasecurity/trivy-action. Its database changes daily, so a pass is not reused."""
    return step(
        name,
        image = "aquasec/trivy:latest",
        run = "trivy fs --scanners {} --severity {} --exit-code 1 {}".format(scanners, severity, target),
        cache = False,
    )

def hadolint(name = "hadolint", dockerfile = "Dockerfile", failure_threshold = "warning"):
    """hadolint/hadolint-action."""
    return step(
        name,
        image = "hadolint/hadolint:latest-debian",
        run = "hadolint --failure-threshold {} {}".format(failure_threshold, dockerfile),
        inputs = [dockerfile],
    )

def docker_build(name, tags, context = ".", dockerfile = "Dockerfile", build_args = {}, needs = []):
    """docker/build-push-action, without the push: build, and check the build works.

    Pushing is something done once, from main, with credentials; see
    branches.ci.star for a step that is never reused because it has effects.
    """
    args = " ".join(["--build-arg {}={}".format(k, v) for k, v in sorted(build_args.items())])
    return step(
        name,
        image = "docker:27-cli",
        run = "docker build {} {} -f {} {}".format(" ".join(["-t " + t for t in tags]), args, dockerfile, context),
        inputs = [context],
        needs = needs,
    )

def ci(ctx):
    test = setup_node("web test", "npm ci && npm test", version = "22", inputs = ["web"])
    checks = [
        test,
        setup_python("api test", "pip install -e . && pytest -q"),
        trivy(severity = "CRITICAL"),
        hadolint(),
    ]
    image = docker_build("image", tags = ["app:ci"], build_args = {"NODE_VERSION": "22"}, needs = checks)
    return checks + [image]
