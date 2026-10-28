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

The pages in `docs/` are built with [Material for MkDocs](https://squidfunk.github.io/mkdocs-material/).
`docs/examples` is a link to the `examples/` directory, so the scripts and what
they compile to are served as they are in the repository.

```sh
uv run --with 'mkdocs-material==9.8.0' --with 'mkdocs<2' mkdocs serve
uv run --with 'mkdocs-material==9.8.0' --with 'mkdocs<2' mkdocs build --strict
```

The build is strict, so a broken link fails it.
