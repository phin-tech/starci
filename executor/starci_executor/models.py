"""The plan the planner emits, as the executor understands it.

`starci plan` (Go) is the only producer of this JSON and already validates it.
It is validated again here because this side is what actually runs containers:
a plan that reaches the executor has crossed a process boundary, and "the other
program checked it" is not a property worth depending on.
"""

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator


class Step(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)

    name: str = Field(min_length=1)
    image: str = Field(min_length=1)
    run: str = Field(min_length=1)
    needs: tuple[str, ...] = ()
    env: dict[str, str] = Field(default_factory=dict)
    # Repository paths the step is given; nothing else is mounted. Its result
    # is keyed on their contents.
    inputs: tuple[str, ...] = (".",)
    # name -> container path, kept between runs on a worker.
    caches: dict[str, str] = Field(default_factory=dict)
    # False for a step that must run every time.
    cache: bool = True

    @field_validator("inputs")
    @classmethod
    def _inside_the_repo(cls, paths: tuple[str, ...]) -> tuple[str, ...]:
        if not paths:
            raise ValueError("inputs must not be empty")
        for p in paths:
            if not p or p.startswith("/") or p == ".." or p.startswith("../") or "/../" in p:
                raise ValueError(f"input {p!r} must be a path inside the repository")
        return paths

    @field_validator("caches")
    @classmethod
    def _mountable(cls, caches: dict[str, str]) -> dict[str, str]:
        for name, path in caches.items():
            if not name.replace("-", "").replace("_", "").isalnum():
                raise ValueError(f"cache name {name!r} may only use letters, digits, '-' and '_'")
            if not path.startswith("/"):
                raise ValueError(f"cache {name!r} needs an absolute path, got {path!r}")
        return caches


class Plan(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)

    repo: str
    sha: str
    ref: str = ""
    steps: tuple[Step, ...]

    @model_validator(mode="after")
    def _runnable(self) -> "Plan":
        names = [s.name for s in self.steps]
        dupes = sorted({n for n in names if names.count(n) > 1})
        if dupes:
            raise ValueError(f"duplicate step names: {', '.join(dupes)}")
        for s in self.steps:
            missing = [n for n in s.needs if n not in names]
            if missing:
                raise ValueError(f"step {s.name!r} needs unknown step(s): {', '.join(missing)}")
        self.layers()  # raises on a cycle
        return self

    def layers(self) -> list[list[Step]]:
        """Steps grouped so that each layer depends only on earlier layers.

        Within a layer, steps keep the order the script returned them. The
        pipeline starts children in exactly this order every time, which is
        what a durable task needs to replay.
        """
        remaining = list(self.steps)
        done: set[str] = set()
        layers: list[list[Step]] = []
        while remaining:
            ready = [s for s in remaining if all(n in done for n in s.needs)]
            if not ready:
                stuck = ", ".join(s.name for s in remaining)
                raise ValueError(f"steps depend on each other in a cycle: {stuck}")
            layers.append(ready)
            done.update(s.name for s in ready)
            remaining = [s for s in remaining if s.name not in done]
        return layers


class PipelineInput(BaseModel):
    plan: Plan
    clone_url: str


class StepInput(BaseModel):
    clone_url: str
    sha: str
    step: Step


class StepResult(BaseModel):
    name: str
    status: Literal["passed", "failed", "skipped"]
    seconds: float = 0.0
    worker: str = ""
    # True when an earlier passing run with the same key stood in for this one.
    cached: bool = False
    detail: str = ""


class PipelineResult(BaseModel):
    ok: bool
    steps: list[StepResult]
