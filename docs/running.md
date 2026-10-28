# Running on every push

Two things turn "something happened in a repository" into a run, and they are
kept apart on purpose.

```
forge ──▶ door ──▶ Hatchet ──event──▶ worker ──▶ the run
                                        │
          pass / fail on the commit ◀───┘
```

**A door** is the address the forge delivers its webhooks to. It relays the
delivery and does nothing else: it has no access to the forge, reads no
script, and can start nothing. So it can be almost anything, and several are
described in `doors/` in the repository:

- nothing at all, with the forge pointed straight at Hatchet's own webhook
  address (`starci hooks add forgejo` sets that up);
- a function on a cloud provider: a Cloudflare Worker and an AWS Lambda are
  provided, about forty lines each;
- `starci serve`, the same thing as a process you run;
- anything that can forward an HTTP request.

**A worker** does the rest, as a task like any other. It checks the forge's
signature itself, fetches `.ci.star` as it is at that commit, evaluates it,
marks the commit `pending`, and starts the run. When the run ends it posts
`success` or `failure`. A script that cannot be evaluated is a failure on the
commit, with the message. A newer push to the same branch cancels the run
before it.

A worker acts on deliveries when it is given the forge's access token and
webhook secret:

| Variable | |
|---|---|
| `STARCI_FORGEJO_URL` | The instance, `https://git.example.com`. |
| `STARCI_FORGEJO_TOKEN_FILE` | File holding an access token: read repositories, write commit statuses. |
| `STARCI_FORGEJO_SECRET_FILE` | File holding the webhook secret. |
| `STARCI_DASHBOARD` | Hatchet's dashboard, as a person reaches it. The status a run posts then links to the run's page there, with each step's output and timing. Optional. |
| `STARCI_TRACES` | `true` to send traces to Hatchet too, where they appear on a run's page. Hatchet needs `SERVER_OBSERVABILITY_ENABLED=t`. Optional. See [Traces](architecture.md#traces). |
| `STARCI_GITHUB_TOKEN_FILE`, `STARCI_GITHUB_SECRET_FILE` | The same, for GitHub. |
| `STARCI_GITHUB_API` | The API's address, for GitHub Enterprise. Default: github.com. |
| `STARCI_ALLOW_FORK_PULL_REQUESTS` | `true` to run pull requests from forks. Off by default. |

With the token, a worker also fetches from private repositories; it is handed
to git through its environment and never appears in a clone URL.

There is no list of triggers to configure. Every event reaches the script,
which says in code what it wants done for it, including nothing.
`starci plan --event tag --tag v1.0.0` shows what a script would do for an
event without causing one.

| Event | The script is told | Status appears as |
|---|---|---|
| A push to a branch | `ctx.event == "push"`, `ctx.ref`, `ctx.changed_files` | `starci` |
| A new tag | `ctx.event == "tag"`, `ctx.tag` | `starci/tag` |
| A pull request opened or updated | `ctx.event == "pull_request"`, `ctx.pr` | `starci/pull-request` |

**Pull requests from forks are not run.** One is code from someone who cannot
push to the repository, and running it means running it on your workers. They
are ignored unless `STARCI_ALLOW_FORK_PULL_REQUESTS=true`; turn that on only
where the workers are meant for strangers' code.

## Schedules

A schedule is the one trigger nothing causes, so it is the one a script
declares:

```python
schedule("nightly", cron = "0 3 * * *")
schedule("backup", cron = "0 2 * * *", overlap = "replace", catch_up = True)

def ci(ctx):
    if ctx.event == "schedule" and ctx.schedule == "nightly":
        return [full_suite, audit]
    ...
```

| | |
|---|---|
| `cron` | Five fields, read in UTC. |
| `overlap` | What happens when it fires while its last run is still going: `"skip"` the new firing (the default), `"replace"` the run in progress, or `"allow"` both. |
| `catch_up` | `True` to run once if a firing was missed because nothing was running to fire it, however many were missed. Default `False`. |

**Only the default branch's schedules count.** Each push to it makes the
schedules that fire match what the script there declares: new ones are added,
changed ones updated, and ones no longer declared removed. A script that
cannot be read changes nothing, so a typo does not cancel tonight's run.

When a schedule fires, the worker looks at the default branch as it stands at
that moment, and asks the script there what the schedule wants done. The
result is posted on that commit as `starci/schedule-<name>`. A schedule whose
repository or script has gone removes itself.

On Hatchet a schedule is a Hatchet cron, visible in its dashboard. When
compiled for GitHub it becomes a `schedule:` trigger, where `overlap` and
`catch_up` do not carry over: GitHub lets scheduled runs overlap and never
catches up.

The record `catch_up` is measured against is kept on the worker that fires
schedules, so for now that is one worker's job.
