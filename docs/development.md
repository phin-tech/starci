# Development

```sh
go test ./...          # the step tests run real containers
go test -short ./...   # without them
```

The engine tests run against the local engine with everything else. An engine
that needs a service to test against runs its tests when asked, with
`STARCI_TEST_<NAME>=1`.

The shared store's tests need an S3, and run when `STARCI_TEST_S3` names a
bucket there; the bucket is made if it is missing. [Floci](https://floci.io), a
local AWS emulator, is enough:

```sh
docker run -d --rm --name floci -p 4566:4566 floci/floci
AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test \
STARCI_TEST_S3='s3://starci-test/v1?endpoint=localhost:4566&insecure=1&region=us-east-1' \
  go test ./internal/store/s3
```

They store and fetch results and blobs, and run steps on two machines that
share nothing but the bucket: one keeps an artifact, the other is handed it,
and a pass on one is reused on the other.

## This documentation

The pages in `docs/` are built with [Zensical](https://zensical.org/), which
reads `mkdocs.yml` and gives the Material for MkDocs look. `docs/examples` is a
copy of `examples/` made by the script, which also writes a page for each
example (`dev/docs-examples.py`): the script, and for a basic one what it
compiles to for GitHub Actions and for CircleCI. A new example has to be added
to the nav in `mkdocs.yml`, and the build says so if it is not. Building needs Go,
since the page of a basic example draws its plan with `starci plan`.

The release processes (`branches`, `events`, `wrapping-release-actions`,
`deploy-gate`, `canary`) are played from drawings in `docs/animations`, with
[dottie.js](https://dottie.phin.tech/dottie-js/); a page gets one by having a
drawing named for its example. See `dev/animations/README.md`.

```sh
dev/docs.sh serve   # while writing
dev/docs.sh         # the same build CI does; a broken link fails it
```

Zensical is before 1.0, so its version is pinned, in `dev/docs.sh` and in
`.github/workflows/docs.yml`.
