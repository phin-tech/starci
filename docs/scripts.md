# What a script can do

| | |
|---|---|
| `step(name, run, image = "", needs = [], env = {}, inputs = ["."], caches = {}, cache = True, on = {}, prefer = {}, avoid = {}, tools = [], packages = [], network = True, services = [])` | Declare a step. `run` is a shell command, or a list of a program and its arguments run without a shell. A step with no `image` is a [bare step](bare.md). Returns a handle usable in another step's `needs`. |
| `service(name, image, env = {}, args = [], ready = "", ready_within = 120, files = {})` | Declare a container to run beside a step, for its `services`. |
| `ctx.repo`, `ctx.sha`, `ctx.ref` | What is being built. `ref` is the branch. |
| `ctx.event` | Why the run started: `"push"`, `"tag"`, `"pull_request"`, `"schedule"` or `"manual"`. |
| `ctx.tag` | The tag's name, for a tag. |
| `ctx.pr` | For a pull request: `.number`, `.base`, `.head`, and `.fork`, true when it comes from another repository. `None` otherwise. |
| `ctx.schedule` | The name of the schedule that fired. |
| `schedule(name, cron, overlap = "skip", catch_up = False)` | Declare a schedule, at the top of the file. A compiled workflow fires it, and so does a CI tool's scheduled job that runs starci, which tells the script which one fired. `overlap` and `catch_up` are kept in the language, and nothing honours them now. |
| `ctx.changed_files` | Files the commit changed, sorted. |
| `ctx.changed_dirs` | Their top-level directories, sorted (`.` for files at the root). |
| `match_files(files, include = [], exclude = [])` | The paths that match a glob in `include` (any, when empty) and none in `exclude`: what `tj-actions/changed-files` is used for. Pass it `ctx.changed_files`. |
| `paths_filter(files, filters)` | A dict with a list of matching paths for each named list of globs: what `dorny/paths-filter` is used for. An empty list is false. |

Globs match the whole path. `*` and `?` stay within a directory, `**` does not,
`**/` may match no directory, `{a,b}` is either, and a pattern starting with
`!` removes what it matches from the rest of its list.

Nothing else. A script cannot read files, reach the network, read the clock or
`load()` another file, and it is stopped if it runs too long. The same script
and inputs always give the same plan, so a plan can be computed in one place
and executed in another, and a script from any repository is safe to evaluate.


## Two modes

A script is in one of two modes, told apart by the function it defines.

**Basic** — `ci(ctx)` returns the steps. The whole graph is known before
anything runs; `starci plan` prints it.

**Advanced** — `pipeline(ctx)` runs steps itself and reacts to what comes back:

```python
def pipeline(ctx):
    run(build)

    # How wide to fan out is read from a step, not written down.
    files = run(list_tests).output.splitlines()
    results = parallel([test(f) for f in files], allow_failure = True)

    # One more try for whatever failed, and only for that.
    failed = [r.name for r in results if not r.ok]
    if failed:
        results = parallel([test(n) for n in failed], allow_failure = True)
    if [r for r in results if not r.ok]:
        fail("failed twice")
```

| | |
|---|---|
| `run(step, allow_failure = False)` | Run one step and return its result. |
| `parallel(steps, allow_failure = False)` | Run several together; results come back in the same order. |
| `result.ok`, `.status`, `.output`, `.name`, `.number`, `.seconds`, `.cached`, `.machine`, `.detail` | What a result carries. `output` is what the step printed, up to 64 KB. `number` is which step of the run it was, counted in the order the script asked: two steps may share a name, never a number. |
| `fail(message)` | End the run as failed. |

A step that fails ends the run, unless `allow_failure` hands the script the
failed result to decide on. Steps are the same `step(...)` as in basic mode,
with the same inputs, skipping and placement; `needs` is not used, since the
script's own order says what runs after what.

On a durable engine, an advanced script would run as a durable task: if the
machine carrying it died partway, another would pick the run up. There is no
durable engine now, so an interrupted advanced run starts again. The language
is ready for one: evaluated again from the top, a script would have each step
it asks for answered from the record of the run, so finished steps would not
be repeated. This is why the language matters. A script that cannot
read a clock or a file asks for the same steps, in the same order, whenever it
is given the same answers.

The trade is predictability. A basic script's graph depends only on the
commit; an advanced script's depends on what its steps return.
