# Plan: `starci compile --target argo`

**Status:** built (`internal/compile/argo.go`). Checked against Argo v3.7.18's
source: `depends`/`dependencies` default to "succeeded or skipped", so a step
this event does not plan (`when` false: Skipped) lets its dependents go ahead
and a failed one makes them Omitted; `when` takes `{{=expr}}` with
`workflow.parameters.*`; a template's `initContainers` are the Kubernetes
container type, so `restartPolicy: Always` and `startupProbe` are accepted;
`retryPolicy: OnError` exists. Argo's own strict decoder and validator accept
every file produced. **Not verified:** that a cluster runs it, in particular
that Argo's executor finishes a pod with a native sidecar still running.
Passes work through `starci pass key|get|put` and a `store` parameter (key checked
locally against a real git remote; the pod wiring not run). **Not built:**
artifacts, a service's `files`.

## Decided

- **A `WorkflowTemplate` with one DAG**, taking the run's context as
  parameters. Submit with `argo submit --from workflowtemplate/NAME -p ...`;
  a `CronWorkflow` can reference it.
- **A task runs in the step's own image.** No Docker, no privilege. An init
  container (a starci image) puts the step's inputs and tools in place; the
  main container runs the step's command. This is the Kubernetes-Jobs shape
  AGENTS.md already sketches, and it means a third copy of machine behaviour
  next to `internal/step` and the CI targets (`startServices`, `bareRun`,
  `artifacts.go`). Each of those must be made to say so.
- Frozen plan, per kind of event, from `eventUnion`, as for GitHub and
  Temporal. Advanced scripts refused.
- Registered in the `targets` table in `cmd/starci/compile.go`; one file
  (`-o FILE` or stdout), `--check FILE`.

## Reuse

Less was tangled in `job()` than feared. Already plain functions and usable as
they are: `gitPathspec`/`sparsePattern` (inputs), `unpackCommand`, `jobIDs`,
`keepLines`/`placeLines`, `commitNote`/`wrap`/`q`/`shq`. The one piece that was
inline in both the GitHub and CircleCI targets, the tool download, is now
`toolInstall` (`internal/compile/tools.go`); the init container uses it.

Not reusable, because they are Docker: `startServices`/`stopServices` (services
here are native sidecars) and the `docker run` assembly. `bareRun` is shell for
a CI machine's own processes; a bare step here runs in `--bare-image`, where
the pod is the isolation, so the command runs directly.

## Per-feature mapping (propose; each may become a v1 refusal)

| Feature | Proposal | To settle |
|---|---|---|
| inputs, checkout | init container: sparse checkout of exactly the step's inputs into an `emptyDir` mounted into the main container | image for the init container; pin by digest |
| tools | init container downloads and verifies checksums onto a shared volume; PATH put in front | same shell as GitHub |
| `network = False` | NetworkPolicy is per pod, and the init container needs the network. *verify* whether this is expressible (e.g. an init container does all fetching, then a policy applies to the pod's main phase is not possible: policies are not per container). Likely **refuse** in v1 | refuse vs. a Cilium/other CNI-specific option |
| `packages` (apt on top of the image) | the GitHub target builds an image; Kubernetes cannot without privilege. apt at start needs root and the network. Likely **refuse** in v1 | refuse, or run apt in the init container into a volume (fragile) |
| `cache = True` passes | Argo `memoize` keys on a string known before the task runs, so it cannot hash contents. Options: a hashing task before each step; a check against `STARCI_STORE` inside the pod; none in v1 | which; the CI targets key on input contents, so "none" changes semantics |
| services | native sidecars: init containers with `restartPolicy: Always` and `startupProbe` from `ready`; names via `hostAliases` for 127.0.0.1 (AGENTS.md). Two services on one port **refused**. *verify* Argo passes native sidecars through and the minimum Kubernetes/Argo versions | classic Argo sidecars do not gate the main container, so they are not an option |
| `files` of a service | ConfigMap/volume read-only in the service's container, not the step's workspace | |
| `on` | `nodeSelector`; `os`/`arch` map to `kubernetes.io/os` and `kubernetes.io/arch` | |
| `prefer`/`avoid` | node affinity, preferred and anti: *not dropped*, unlike the other targets | |
| bare steps (no image) | refuse, or a default image + `--bare-image`-like flag | |
| artifacts | Argo's own artifact passing between DAG tasks can resolve `artifact()` placeholders statically, as the GitHub target does. Needs an artifact repository configured in the cluster. Could work in v1 | v1 or v2 |
| `keep` | output artifacts of the task | |
| caches (`caches`) | PVC or emptyDir per task; emptyDir loses them | |

## DAG semantics (verify before writing)

- A step absent for this event must let its dependents run; a failed step must
  skip them, with a result shaped like `next`'s. *verify* whether
  `depends: "a.Succeeded || a.Skipped"` gives both, and what Argo calls a
  dependent of a failed task (Omitted vs Skipped).
- Conditions: Argo's `when` cannot parse a JSON context. Take flat parameters
  (`event`, `ref`, `fork`, `schedule`) and add an `argoWhen` field to `probe`
  beside `goWhen`, passed to `eventUnion`'s `condition`. *verify* string
  comparison quoting in `when`.
- Retries: a failed step is a result and must not be retried by Argo
  (`retryStrategy` off, or limited to infrastructure failures such as node
  loss).

## Pins

- Argo `apiVersion` for `WorkflowTemplate`, and the minimum Argo version.
- Minimum Kubernetes for native sidecars.
- The init container's image: a starci image published by someone; pin by
  digest. Open: who builds and publishes it.

## Tests

- Goldens under `examples/basic/compiled/argo/<name>.yml`, written and
  checked by `compile.sh`.
- Validate offline against Argo's CRD schema (`argo lint --offline` if the
  version has it, else kubeconform).
- A real run on kind or k3d behind `STARCI_TEST_ARGO`, documented in
  `docs/development.md`, in the way `run_test.go` carries out compiled
  workflows today.

## Settled

1. **`network = False` and `packages`: refused**, with a compile error naming
   the step. A pod's network policy covers the whole pod, including the init
   container; installing packages needs root and the network.
2. **Passes: an in-pod check against `STARCI_STORE`.** The init container
   hashes the inputs the way a machine does and asks the shared store; a hit
   marks the task passed without running the command. `STARCI_STORE` must be
   reachable from the cluster, and the header says so.
3. **Artifacts: refused in v1**, as for Temporal. v2 decides between Argo's own
   artifacts and the store.
4. **The init image is a required flag, `--init-image REF`**, with no default.
   starci publishes nothing and names no registry (ADR 0001). It needs git,
   curl/tar, sha256sum and whatever the passes check calls. A Dockerfile under
   `dev/` can build one.
5. **Bare steps: `--bare-image REF`** supplies the image they run in, as
   `starci run`'s flag does; without it a bare step is refused. The pod is the
   isolation.

## Next

1. ~~Extract the tool download~~ (done: `toolInstall`; the GitHub goldens are
   unchanged, the CircleCI ones now quote the download path).
2. Add `argoWhen` to `probe`, and the Argo target in `internal/compile/argo.go`.
3. Register it in `cmd/starci/compile.go` (`--init-image`, `--bare-image`).
4. Goldens, offline schema validation, docs, and a gated kind/k3d run.
