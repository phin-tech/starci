"""Plan a commit and run it.

    uv run python -m starci_executor.trigger CHECKOUT --clone-url URL [--sha SHA] [--file .ci.star]

Evaluates the script with the Go planner, validates the plan, hands it to the
ci-pipeline task and waits for the result.
"""

import argparse
import os
import subprocess
import sys
import time
from pathlib import Path

from .models import PipelineInput, Plan

REPO_ROOT = Path(__file__).resolve().parents[2]


def git(checkout: str, *args: str) -> str:
    return subprocess.run(
        ["git", "-C", checkout, *args], check=True, capture_output=True, text=True
    ).stdout.strip()


def make_plan(checkout: str, script: Path, repo: str, sha: str) -> Plan:
    changed = git(checkout, "diff-tree", "--no-commit-id", "--name-only", "-r", sha).split()
    planner = os.environ.get("STARCI_BIN")
    cmd = [planner] if planner else ["go", "run", "./cmd/starci"]
    out = subprocess.run(
        [*cmd, "plan", "--file", str(script), "--repo", repo, "--sha", sha, "--changed", ",".join(changed)],
        cwd=REPO_ROOT,
        capture_output=True,
        text=True,
    )
    if out.returncode != 0:
        sys.exit(out.stderr.strip())
    return Plan.model_validate_json(out.stdout)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("checkout", help="local checkout of the repository")
    parser.add_argument("--clone-url", required=True, help="URL workers clone from")
    parser.add_argument("--sha", help="commit to build (default: HEAD of the checkout)")
    parser.add_argument("--file", help="CI script (default: .ci.star in the checkout)")
    parser.add_argument("--repo", help="owner/name, for display (default: from the clone URL)")
    args = parser.parse_args()

    sha = git(args.checkout, "rev-parse", args.sha or "HEAD")
    script = Path(args.file).resolve() if args.file else Path(args.checkout, ".ci.star").resolve()
    repo = args.repo or args.clone_url.removesuffix(".git").split("/", 3)[-1]

    plan = make_plan(args.checkout, script, repo, sha)
    print(f"{plan.repo} @ {plan.sha[:8]}: {len(plan.steps)} step(s)")
    for layer in plan.layers():
        print("  " + "  |  ".join(s.name for s in layer))
    if not plan.steps:
        print("nothing to run")
        return

    # Imported late: constructing the Hatchet client needs credentials, and
    # planning should not.
    from .worker import ci_pipeline

    started = time.monotonic()
    result = ci_pipeline.run(PipelineInput(plan=plan, clone_url=args.clone_url))
    elapsed = time.monotonic() - started

    print()
    for r in result.steps:
        where = f" on {r.worker}" if r.worker else ""
        note = f"cached, first passed{where}" if r.cached else f"{r.seconds}s{where}"
        print(f"  {r.status:8} {r.name}  {note}")
        if r.detail:
            for line in r.detail.splitlines()[-15:]:
                print(f"           {line}")
    print(f"\n{'PASSED' if result.ok else 'FAILED'} in {elapsed:.1f}s")
    sys.exit(0 if result.ok else 1)


if __name__ == "__main__":
    main()
