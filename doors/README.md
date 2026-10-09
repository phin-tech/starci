# Doors

A door is the address a forge delivers its webhooks to. It is deliberately the
least interesting part of starci: it relays a delivery, and a worker does
everything else.

```
forge ──▶ door ──▶ Hatchet's webhook address ──▶ event ──▶ starci worker
                   (checks the signature)                  (fetches the script,
                                                            plans, runs)
```

Because a door does so little, it can be almost anything. Pick whichever suits
where the forge is and what you already run.

| Door | Use it when |
|---|---|
| **None.** Point the forge straight at Hatchet. | The forge can reach Hatchet: both on one network, or Hatchet is hosted. |
| [`cloudflare-worker/`](cloudflare-worker/worker.js) | The forge is public and you would rather not expose Hatchet's address to it directly. |
| [`aws-lambda/`](aws-lambda/handler.py) | The same, on AWS. |
| `starci serve` | You want to run the door yourself as a process. It speaks to Hatchet directly rather than relaying over HTTP. |
| Anything that can forward an HTTP request | A reverse proxy rule, an API gateway, a tunnel with one path allowed. |

## The contract

A door receives the forge's `POST` and sends the same request on to Hatchet's
webhook address for that forge:

1. **The body, byte for byte.** The forge's signature is over it. A door that
   parses and re-serialises the JSON breaks the signature.
2. **The signature header**, whichever the forge sent: `X-Forgejo-Signature`,
   `X-Gitea-Signature` or `X-Hub-Signature-256`.
3. **The event header**, if it is easy to: `X-Forgejo-Event`, `X-Gitea-Event`
   or `X-GitHub-Event`. A worker can tell a push from a pull request without
   it.
4. **`Content-Type: application/json`.**

It answers the forge with success if Hatchet did, and with a failure if not,
so that the forge retries what is worth retrying.

That is all of it. A door needs no access to the forge and none to the
repository. It reads no script and decides nothing.

### What a door may also do

Check the signature itself, if given the webhook secret, and turn unsigned
deliveries away. This is a kindness to Hatchet, not a safeguard: Hatchet checks
the signature regardless, so a door that checks nothing, or checks wrongly,
lets nothing through that should not get through.

## Setting one up

1. Give the forge and Hatchet a shared secret, and register the webhook with
   Hatchet:

   ```sh
   export STARCI_FORGEJO_URL=https://git.example.com
   export STARCI_FORGEJO_SECRET_FILE=/path/to/webhook-secret
   starci hooks add forgejo
   ```

   It prints the address Hatchet now accepts deliveries at.
2. Deploy a door with that address as `HATCHET_WEBHOOK_URL`, or skip the door
   and use the address directly.
3. In the repository's settings on the forge, add a webhook: the door's
   address, content type JSON, push and pull request events, the same secret.
4. Run a worker with the forge's access token and the same secret, so that it
   can fetch scripts and post results.

## Tests

```sh
doors/test.sh
```

runs both reference doors against a stand-in for Hatchet: a signed delivery is
relayed unchanged, an unsigned one is turned away when the door has the
secret and relayed when it does not, and Hatchet's refusals and failures are
passed back.
