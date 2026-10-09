# Infrastructure code: check formatting and validate each stack that changed.
#
# A stack is a directory under stacks/. Its step is given that stack and the
# shared modules, so a stack nobody touched keeps its earlier result.

TF = "hashicorp/terraform:1.9"

def ci(ctx):
    steps = [step("fmt", image = TF, run = "terraform fmt -check -recursive", inputs = ["**/*.tf"])]

    stacks = []
    for f in ctx.changed_files:
        parts = f.split("/")
        if parts[0] == "stacks" and len(parts) > 2 and parts[1] not in stacks:
            stacks.append(parts[1])

    for stack in sorted(stacks):
        steps.append(step(
            "validate " + stack,
            image = TF,
            run = "cd stacks/{} && terraform init -backend=false -input=false && terraform validate".format(stack),
            inputs = ["stacks/" + stack, "modules"],
            caches = {"terraform-plugins": "/root/.terraform.d/plugin-cache"},
            env = {"TF_PLUGIN_CACHE_DIR": "/root/.terraform.d/plugin-cache"},
        ))
    return steps
