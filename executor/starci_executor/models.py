"""The plan the planner emits, as the executor understands it.

`starci plan` (Go) is the only producer of this JSON and already validates it.
It is validated again here because this side is what actually runs containers:
a plan that reaches the executor has crossed a process boundary, and "the other
program checked it" is not a property worth depending on.
"""

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


class Step(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)

    name: str = Field(min_length=1)
    image: str = Field(min_length=1)
    run: str = Field(min_length=1)
    needs: tuple[str, ...] = ()
    env: dict[str, str] = Field(default_factory=dict)


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
    detail: str = ""


class PipelineResult(BaseModel):
    ok: bool
    steps: list[StepResult]
