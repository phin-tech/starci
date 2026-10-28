# Trying it

Needs Go, Docker and git.

```sh
go build -o starci ./cmd/starci

./starci run ~/src/pi-gamebook --local --file examples/basic/pi-gamebook.ci.star
```

```
src/pi-gamebook @ e8da515a: 2 step(s)
  check  |  test

  passed   check  2.9s on this machine
  passed   test  2.0s on this machine

PASSED in 2.9s
```

`--local` builds what is committed, fetched from the checkout itself, in the
same sandbox and with the same result cache as a worker. `-v` prints each
step's output as it runs; Ctrl-C stops the steps and removes their containers.
There is no queue and nothing to resume from: it is for trying a script, not
for running CI.

## On Hatchet

```sh
docker compose -f dev/docker-compose.yml -p starci-dev up -d   # throwaway Hatchet
dev/token.sh > dev/.env                                        # credentials for it
set -a; . dev/.env; set +a

./starci worker &
./starci run ~/src/pi-gamebook \
  --clone-url https://git.phin.tech/sam/pi-gamebook.git \
  --file examples/basic/pi-gamebook.ci.star
```

The dashboard at http://localhost:18888 (admin@example.com / Admin123!!) shows
each run as a `ci-pipeline` (basic) or `ci-script` (advanced) parent with one
`ci-step` child per step.
