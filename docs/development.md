# Development

```sh
go test ./...          # the step tests run real containers
go test -short ./...   # without them
```

The engine tests run against the local engine with everything else. Against
Hatchet they need one to talk to, so they run when asked, with this process
as the worker:

```sh
set -a; . dev/.env; set +a
STARCI_TEST_HATCHET=1 go test ./internal/engine/hatchet -run TestEngine
```

## This documentation

The pages in `docs/` are built with [Zensical](https://zensical.org/), which
reads `mkdocs.yml` and gives the Material for MkDocs look. `docs/examples` is a
copy of `examples/` made by the script, so the example scripts and what they
compile to are served as they are in the repository.

```sh
dev/docs.sh serve   # while writing
dev/docs.sh         # the same build CI does; a broken link fails it
```

Zensical is before 1.0, so its version is pinned, in `dev/docs.sh` and in
`.github/workflows/docs.yml`.
