# starci

CI whose pipelines are Starlark scripts. starci decides what a run is; an engine gets its steps onto machines.

## Language

### Running

**Engine**:
What gets a run's steps onto machines and waits for them. The local engine, which runs everything on the machine it is on, is the one there is.
_Avoid_: backend, executor, runner

**Machine**:
Whatever runs one step: the computer `starci run` is on, a CI tool's runner, and for another engine a Lambda invocation or a pod.
_Avoid_: worker, host, runner, node

**Placement**:
What a step says about the machine it needs (`on`, `prefer`, `avoid`). Well-known keys (`os`, `arch`) mean the same on every engine; any other key is one engine's own, and an engine that cannot honour a key says so before the run starts.
_Avoid_: labels (for the step's side), constraints

### Steps

**Bare step**:
A step that names no image, and runs as the machine's own processes, in whatever the machine provides.
_Avoid_: host step, native step
