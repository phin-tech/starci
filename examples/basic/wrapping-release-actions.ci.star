# Wrapping the release actions: docker/login-action with docker/build-push-action,
# and goreleaser/goreleaser-action. Releases are where a CI workflow has effects,
# so every wrapper here is `cache = False` (a pass says nothing about a
# publish that must happen) and the steps exist only for a tag.
#
# starci has no way yet to hand a step a secret. The empty `env` values below
# only name what each step needs; getting a real value to them is up to
# whatever runs the step, so treat this as the shape of the wrapper and not as
# something to run as it is.

def docker_push(image, tags, registry = "ghcr.io", needs = []):
    """docker/login-action followed by docker/build-push-action."""
    full = ["{}/{}:{}".format(registry, image, t) for t in tags]
    return step(
        "push image",
        image = "docker:27-cli",
        run = "echo \"$REGISTRY_TOKEN\" | docker login {} -u \"$REGISTRY_USER\" --password-stdin && docker build {} . && {}".format(
            registry,
            " ".join(["-t " + t for t in full]),
            " && ".join(["docker push " + t for t in full]),
        ),
        env = {"REGISTRY_USER": "", "REGISTRY_TOKEN": ""},
        cache = False,
        needs = needs,
    )

def goreleaser(version = "~> v2", args = "release --clean", needs = []):
    """goreleaser/goreleaser-action."""
    return step(
        "goreleaser",
        image = "goreleaser/goreleaser:latest",
        run = "goreleaser " + args,
        env = {"GITHUB_TOKEN": ""},
        cache = False,
        needs = needs,
    )

def ci(ctx):
    test = step("test", image = "golang:1.26", run = "go test ./...", inputs = ["**/*.go", "go.mod", "go.sum"])
    steps = [test]
    if ctx.event == "tag":
        # v1.2.3 also moves the 1.2 and 1 tags a user may be following.
        version = ctx.tag.lstrip("v")
        tags = [version, ".".join(version.split(".")[:2]), version.split(".")[0], "latest"]
        steps += [
            docker_push("sam/app", tags, needs = [test]),
            goreleaser(needs = [test]),
        ]
    return steps
