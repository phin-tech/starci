"""A door for starci, as an AWS Lambda behind a function URL or an HTTP API.

It relays a forge's webhook delivery to Hatchet's webhook address, and that is
all. Hatchet checks the signature and raises the event; a starci worker does
the rest. See ../README.md for the contract.

Environment:
  HATCHET_WEBHOOK_URL  where to relay to, as printed by `starci hooks add`
  WEBHOOK_SECRET       optional. With it, deliveries that are not signed by
                       the forge are turned away here, before they cost
                       Hatchet anything. Hatchet checks either way.

No dependencies beyond the standard library.
"""

import base64
import hashlib
import hmac
import os
import urllib.error
import urllib.request

# The headers a delivery's meaning and proof travel in. Nothing else is
# passed on.
RELAYED = (
    "content-type",
    "x-forgejo-event", "x-forgejo-signature",
    "x-gitea-event", "x-gitea-signature",
    "x-github-event", "x-hub-signature-256",
)
SIGNATURES = ("x-forgejo-signature", "x-gitea-signature", "x-hub-signature-256")


def handler(event, context=None):
    method = event.get("requestContext", {}).get("http", {}).get("method", "POST")
    if method != "POST":
        return _reply(405, "deliveries are POSTed here")

    # The signature is over the body exactly as sent, so it is kept as bytes.
    raw = event.get("body") or ""
    body = base64.b64decode(raw) if event.get("isBase64Encoded") else raw.encode()
    headers = {k.lower(): v for k, v in (event.get("headers") or {}).items()}

    secret = os.environ.get("WEBHOOK_SECRET", "")
    if secret and not _signed(headers, body, secret):
        return _reply(401, "the delivery is not signed with the shared secret")

    request = urllib.request.Request(
        os.environ["HATCHET_WEBHOOK_URL"],
        data=body,
        method="POST",
        headers={name: headers[name] for name in RELAYED if headers.get(name)},
    )
    try:
        with urllib.request.urlopen(request, timeout=20):
            return _reply(202, "accepted")
    except urllib.error.HTTPError as err:
        # Hatchet's answer is the forge's answer: 403 for a bad signature, a
        # server error for one worth retrying.
        return _reply(err.code, "refused")
    except OSError:
        # A failure the forge will see, and try again after.
        return _reply(502, "could not reach hatchet")


def _signed(headers, body, secret):
    given = next((headers[name] for name in SIGNATURES if headers.get(name)), "")
    given = given.removeprefix("sha256=")
    expected = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(given, expected)


def _reply(status, text):
    return {"statusCode": status, "headers": {"content-type": "text/plain"}, "body": text + "\n"}
