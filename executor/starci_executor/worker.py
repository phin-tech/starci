"""Hatchet tasks that execute a plan.

Two generic tasks, rather than one registered workflow per repository, because
the shape of a run is only known once its script has been evaluated:

  ci-step      runs one container against a fresh checkout of the commit
  ci-pipeline  walks a plan and starts a ci-step child per step

Run a worker with:  uv run python -m starci_executor.worker
"""

import asyncio
import hashlib
import json
import os
import socket
import subprocess
import tempfile
import time
from collections import deque
from pathlib import Path

from dotenv import load_dotenv
from hatchet_sdk import Context, DurableContext, Hatchet

from .models import PipelineInput, PipelineResult, Step, StepInput, StepResult

load_dotenv()
hatchet = Hatchet()

WORKER_NAME = os.environ.get("STARCI_WORKER_NAME", socket.gethostname())
# How much of a failed step's output is carried in its error, for the caller.
# The full output is in the task's logs.
TAIL_LINES = 40
# Passing results, one file per key. Per worker for now: a result recorded on
# one machine is not seen by another.
RESULTS_DIR = Path(os.environ.get("STARCI_STATE", Path.home() / ".cache" / "starci")) / "results"


class StepFailed(Exception):
    pass


class Checkout:
    """One commit, fetched into a git directory that is never mounted.

    Keeping the git directory out of the workspace is what makes `inputs` a
    real boundary: a shallow fetch still holds every file of the commit, so a
    workspace containing .git would let a step read what it did not declare.
    """

    def __init__(self, clone_url: str, sha: str, root: str) -> None:
        self.git_dir = os.path.join(root, "git")
        self.workspace = os.path.join(root, "workspace")
        os.makedirs(self.workspace)
        subprocess.run(["git", "init", "-q", "--bare", self.git_dir], check=True, capture_output=True, text=True)
        # Fetch exactly the commit rather than cloning a branch: the branch may
        # have moved on by the time this step runs.
        self._git("fetch", "-q", "--depth", "1", clone_url, sha)

    def _git(self, *args: str) -> str:
        return subprocess.run(
            ["git", f"--git-dir={self.git_dir}", f"--work-tree={self.workspace}", *args],
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()

    def content_ids(self, paths: tuple[str, ...]) -> list[tuple[str, str]]:
        """Git's own hash of each input: a blob id for a file, a tree id for a
        directory. Two commits with identical content under a path share it."""
        ids = []
        for p in paths:
            spec = "FETCH_HEAD^{tree}" if p == "." else f"FETCH_HEAD:{p}"
            try:
                ids.append((p, self._git("rev-parse", "--verify", "-q", spec)))
            except subprocess.CalledProcessError:
                raise StepFailed(f"input {p!r} does not exist in this commit") from None
        return ids

    def materialise(self, paths: tuple[str, ...]) -> None:
        self._git("checkout", "-q", "FETCH_HEAD", "--", *paths)


def _image_id(image: str) -> str:
    """The content id of the image that will actually run, pulling it if
    needed. A tag can be re-pointed; this cannot."""

    def inspect() -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["docker", "image", "inspect", "--format", "{{.Id}}", image], capture_output=True, text=True
        )

    found = inspect()
    if found.returncode != 0:
        pulled = subprocess.run(["docker", "pull", "-q", image], capture_output=True, text=True)
        if pulled.returncode != 0:
            raise StepFailed(f"cannot pull image {image}: {pulled.stderr.strip()}")
        found = inspect()
    return found.stdout.strip()


def _key(step: Step, image_id: str, content: list[tuple[str, str]]) -> str:
    """Everything that decides a step's outcome, as far as starci can know.
    The step's name is deliberately absent: renaming a step is not a reason to
    run it again."""
    material = json.dumps([image_id, step.run, sorted(step.env.items()), content], separators=(",", ":"))
    return hashlib.sha256(material.encode()).hexdigest()


def _result_path(key: str) -> Path:
    return RESULTS_DIR / key[:2] / f"{key}.json"


@hatchet.task(name="ci-step", input_validator=StepInput, execution_timeout="30m")
def ci_step(input: StepInput, ctx: Context) -> StepResult:
    step = input.step
    started = time.monotonic()

    with tempfile.TemporaryDirectory(prefix="starci-") as root:
        try:
            checkout = Checkout(input.clone_url, input.sha, root)
        except subprocess.CalledProcessError as e:
            raise StepFailed(f"fetch of {input.sha} failed: {e.stderr.strip()}") from e

        key = _key(step, _image_id(step.image), checkout.content_ids(step.inputs))
        stored = _result_path(key)
        if step.cache and stored.exists():
            earlier = StepResult.model_validate_json(stored.read_text())
            ctx.log(f"same image, command and inputs as a passing run on {earlier.worker}; not run again")
            return earlier.model_copy(update={"name": step.name, "cached": True, "seconds": 0.0})

        checkout.materialise(step.inputs)

        cmd = ["docker", "run", "--rm", "-v", f"{checkout.workspace}:/workspace", "-w", "/workspace"]
        for name, path in sorted(step.caches.items()):
            cmd += ["-v", f"starci-cache-{name}:{path}"]
        for name, value in sorted(step.env.items()):
            cmd += ["-e", f"{name}={value}"]
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

    result = StepResult(name=step.name, status="passed", seconds=seconds, worker=WORKER_NAME)
    if step.cache:
        # Only passes are remembered. A failure is always worth running again.
        stored.parent.mkdir(parents=True, exist_ok=True)
        stored.write_text(result.model_dump_json())
    return result


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
