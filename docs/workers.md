# Workers

A worker is the binary plus Docker, git, and a Hatchet token. It needs to
reach the Hatchet engine and the git server; nothing connects in to it.

| Variable | |
|---|---|
| `HATCHET_CLIENT_TOKEN` | Credentials for the Hatchet tenant. |
| `STARCI_WORKER_NAME` | Name recorded in results. Default: the hostname. |
| `STARCI_SLOTS` | Steps to run at once. Default: 4. |
| `STARCI_LABELS` | What the machine offers, as `gpu=true,arch=arm64`. |
| `STARCI_STATE` | Where this machine keeps results and tools. Default: `~/.cache/starci`. |
| `STARCI_STORE` | A result store shared with other machines. Optional. |

## Placing steps

| On a step | |
|---|---|
| `worker = {"gpu": "true"}` | Only on a worker with that label. Waits until one is free. |
| `prefer = {"speed": "fast"}` | On such a worker when one is free, on any other when not. |
| `avoid = {"network": "home"}` | Never on a worker with that label. |

`starci run` warns before submitting when no connected worker can take a step.

## Looking after workers

```sh
starci workers          # who is connected, and what they offer
starci shun luigi       # finish what it has, take nothing new
starci unshun luigi
```

## Superseded runs

A run for a branch replaces the run before it: starting one cancels any still
in progress for the same repository and branch, and stops their containers.
Runs for different branches, or for a bare commit, leave each other alone.
