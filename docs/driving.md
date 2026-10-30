# Carrying a run out from another tool

`starci run` decides what a run does and carries it out, on the machine it is
on. The two halves are also commands of their own, so that something else can
carry a run out: a Temporal workflow, a Hatchet DAG, a Kubernetes controller,
or a loop in a CI job.

- `starci next CHECKOUT` says what to run, given the results so far.
- `starci step` runs one step on the machine it is on and prints its result.

Whatever calls them keeps the results. That list is the run's record: hand it
to `next` again after a machine is lost, and the run carries on without
running what already finished.

## next

`next` reads what is done so far on stdin and prints its answer on stdout,
both JSON. The first time, give it nothing (or `{}`): it works out the run's
context from the checkout and the CI tool it is in, as `starci run` does, and
prints it. After that, hand back the `context` and `clone_url` it printed with
the results so far, so that every call tells the script the same thing,
whichever machine makes it.

```json
{"context": {...}, "clone_url": "...", "results": [{"number": 1, ...}]}
```

It answers:

| Field | |
|---|---|
| `run` | The steps to run now, each with its `number` and the `step` itself: every step the run has asked for with no result yet. Asking again before they finish returns them again; skip the numbers already started. |
| `done`, `ok` | Whether the run is over, and whether it passed. |
| `error` | Why it ended early: a failed step the script did not allow for, its own `fail()`, a mistake in it, or results that do not fit it. |
| `results` | The results so far by number, with the steps a basic plan skipped because something they need did not pass. |
| `context`, `clone_url` | What to hand back. |

It runs nothing, and an advanced script is evaluated from the top each time,
answered from the results it is given. That is quick while the script only
decides what runs; work belongs in a step.

## step

`step` reads one step as `next` asked for it, with the run's context and
clone URL, and runs it as a machine running `starci run` would: in its image,
or bare, given its inputs and services. Each line the step prints goes to
stderr as it comes, for whatever runs `step` to keep. Its result is printed on
stdout, carrying the step's number; add it to the results.

```json
{"step": {"number": 2, "step": {...}}, "context": {...}, "clone_url": "...", "trace": {"traceparent": "..."}}
```

`trace` is optional, and places the step's spans in a run's trace. So is
`given`: the results, by step name, of the steps whose artifacts the step is
handed, for a step that still has `artifact()` placeholders because it was
not handed out by `next` (a compiled Temporal workflow hands steps out this
way). `step` puts the real pointers in their place. A step
handed [artifacts](scripts.md#handing-files-on) fetches them from the store,
so machines that run steps of one run need a shared one (`STARCI_STORE`)
unless they share a state directory. It exits
0 whenever it ran the step, passed or failed: a failure is a result. It fails
only when it could not run the step at all. `--bare`, `--bare-image`,
`STARCI_STATE` and `STARCI_STORE` mean what they mean to `starci run`.

## Steps are known by number

Each step a run asks for takes the next number, in the order the script asks
for them, or for a basic plan in the order it lists them. Keep each result as
`step` printed it: its number and fingerprint are how `next` matches it to
the step it answers. An advanced script may give two steps one name, never
one number. If evaluating the script again asks for a different step at a
number that already has a result, `next` ends the run: the script did not
ask for the same steps given the same results, which resuming depends on.

## A loop in the shell

The smallest thing that carries a run out, one step at a time, keeping its
record in a file. Run it again after it is stopped and it carries on.

```bash
#!/usr/bin/env bash
# Carries out a run with starci next and starci step, one step at a time.
# The state file is the run's record: run this again and it carries on.
set -euo pipefail
starci=${STARCI:-starci}
checkout=${1:-.}
state=${2:-run.json}
[ -s "$state" ] || echo '{}' > "$state"

while :; do
  answer=$("$starci" next "$checkout" < "$state")
  if [ "$(jq .done <<<"$answer")" = true ]; then
    jq -c '{ok, error, results: [.results[] | {number, name, status}]}' <<<"$answer"
    break
  fi
  # Hand back what next worked out, every time.
  jq -c --argjson a "$answer" '. + {context: $a.context, clone_url: $a.clone_url}' "$state" > "$state.new"
  mv "$state.new" "$state"
  jq -c '.run[] as $s | {step: $s, context, clone_url}' <<<"$answer" | while read -r job; do
    result=$("$starci" step --bare <<<"$job")
    jq -c --argjson r "$result" '.results += [$r]' "$state" > "$state.new"
    mv "$state.new" "$state"
  done
done
```

An engine that keeps its own history (Temporal, Hatchet) keeps the results
there instead, runs each step `next` returns as one of its own tasks, and
can run them at once.
