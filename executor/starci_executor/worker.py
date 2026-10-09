"""Hatchet tasks that execute a plan.

Two generic tasks, rather than one registered workflow per repository, because
the shape of a run is only known once its script has been evaluated:

  ci-step      runs one container against a fresh checkout of the commit
  ci-pipeline  walks a plan and starts a ci-step child per step

Run a worker with:  uv run python -m starci_executor.worker
"""

import asyncio
import os
import socket
import subprocess
import tempfile
import time
from collections import deque

from dotenv import load_dotenv
from hatchet_sdk import Context, DurableContext, Hatchet

from .models import PipelineInput, PipelineResult, StepInput, StepResult

load_dotenv()
hatchet = Hatchet()

WORKER_NAME = os.environ.get("STARCI_WORKER_NAME", socket.gethostname())
# How much of a failed step's output is carried in its error, for the caller.
# The full output is in the task's logs.
TAIL_LINES = 40


class StepFailed(Exception):
    pass


def _checkout(clone_url: str, sha: str, dest: str) -> None:
    def git(*args: str) -> None:
        subprocess.run(["git", "-C", dest, *args], check=True, capture_output=True, text=True)

    # Fetch exactly the commit rather than cloning a branch: the branch may
    # have moved on by the time this step runs.
    git("init", "-q")
    git("remote", "add", "origin", clone_url)
    git("fetch", "-q", "--depth", "1", "origin", sha)
    git("checkout", "-q", "FETCH_HEAD")


@hatchet.task(name="ci-step", input_validator=StepInput, execution_timeout="30m")
def ci_step(input: StepInput, ctx: Context) -> StepResult:
    step = input.step
    started = time.monotonic()

    with tempfile.TemporaryDirectory(prefix="starci-") as workspace:
        try:
            _checkout(input.clone_url, input.sha, workspace)
        except subprocess.CalledProcessError as e:
            raise StepFailed(f"checkout of {input.sha} failed: {e.stderr.strip()}") from e

        cmd = ["docker", "run", "--rm", "-v", f"{workspace}:/workspace", "-w", "/workspace"]
        for key, value in sorted(step.env.items()):
            cmd += ["-e", f"{key}={value}"]
        cmd += [step.image, "sh", "-c", step.run]

        tail: deque[str] = deque(maxlen=TAIL_LINES)
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        assert proc.stdout is not None
        for line in proc.stdout:
            line = line.rstrip("\n")
            tail.append(line)
            # Hatchet rejects an empty log message; a blank line still matters
            # for reading test output, so send it as a space.
            ctx.log(line or " ")
        code = proc.wait()

    seconds = round(time.monotonic() - started, 1)
    if code != 0:
        # Raising is what marks the task failed in Hatchet, so a broken step is
        # red in the dashboard instead of a green task carrying a bad exit code.
        raise StepFailed(f"exit code {code} after {seconds}s\n" + "\n".join(tail))
    return StepResult(name=step.name, status="passed", seconds=seconds, worker=WORKER_NAME)


def _reason(error: BaseException) -> str:
    """What a failed child said, without the worker's traceback around it."""
    text = str(error)
    marker = f"{StepFailed.__name__}: "
    return text.rsplit(marker, 1)[-1].strip() if marker in text else text.strip()


@hatchet.durable_task(name="ci-pipeline", input_validator=PipelineInput, execution_timeout="1h")
async def ci_pipeline(input: PipelineInput, ctx: DurableContext) -> PipelineResult:
    plan = input.plan
    results: dict[str, StepResult] = {}

    # One layer at a time, children started in plan order. A step could start
    # the moment its own dependencies finish rather than when its whole layer
    # is ready; this trades that for a start order that never varies.
    for layer in plan.layers():
        runnable = []
        for step in layer:
            blocked = [n for n in step.needs if results[n].status != "passed"]
            if blocked:
                results[step.name] = StepResult(
                    name=step.name, status="skipped", detail=f"needs {', '.join(blocked)}"
                )
            else:
                runnable.append(step)

        outcomes = await asyncio.gather(
            *(
                ci_step.aio_run(StepInput(clone_url=input.clone_url, sha=plan.sha, step=step))
                for step in runnable
            ),
            return_exceptions=True,
        )
        for step, outcome in zip(runnable, outcomes):
            if isinstance(outcome, BaseException):
                results[step.name] = StepResult(name=step.name, status="failed", detail=_reason(outcome))
            else:
                results[step.name] = outcome

    ordered = [results[s.name] for s in plan.steps]
    return PipelineResult(ok=all(r.status == "passed" for r in ordered), steps=ordered)


def main() -> None:
    worker = hatchet.worker(
        f"starci-{WORKER_NAME}",
        slots=int(os.environ.get("STARCI_SLOTS", "4")),
        workflows=[ci_step, ci_pipeline],
    )
    worker.start()


if __name__ == "__main__":
    main()
