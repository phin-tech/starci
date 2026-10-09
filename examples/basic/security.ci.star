# Checks that ask "is anything in here dangerous": leaked secrets, known
# vulnerable dependencies, and a risky Dockerfile.
#
# The dependency audit talks to a vulnerability database that changes daily,
# so its result is never reused: yesterday's pass says nothing about today.

def ci(ctx):
    return [
        step(
            "secrets",
            image = "zricethezav/gitleaks:latest",
            run = "gitleaks dir . --no-banner --redact",
        ),
        step(
            "dependencies",
            image = "aquasec/trivy:latest",
            run = "trivy fs --scanners vuln --exit-code 1 --severity HIGH,CRITICAL .",
            inputs = ["**/package-lock.json", "**/go.sum", "**/uv.lock"],
            cache = False,
        ),
        step(
            "dockerfile",
            image = "hadolint/hadolint:latest-debian",
            run = "hadolint Dockerfile",
            inputs = ["Dockerfile"],
        ),
    ]
