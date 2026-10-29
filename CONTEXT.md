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

**Step number**:
Which step of its run a step is, counted in the order the run asks for them. A step is known by its number; its name is for people, and need not be unique in an advanced script.
_Avoid_: index, sequence, step id

**Artifact**:
A file or directory a step keeps, so that a later step can be handed it. A step sees another's artifact only when the script names it.
_Avoid_: output (that is what a step printed), product, files

### Store

**Store**:
Where results, outputs and artifacts are kept, by the hash of their content: a directory on the machine, or a shared bucket.
_Avoid_: cache (a step's caches are something else), CAS, blob store

**Pointer**:
What a result carries in place of an output or artifact: the hash and size of something in the store.
_Avoid_: ref, handle, digest
