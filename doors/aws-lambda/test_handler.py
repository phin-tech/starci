import base64
import hashlib
import hmac
import json
import os
import sys

import handler

hatchet, secret, log = sys.argv[1:4]
# Odd spacing and a non-ASCII character: a door that re-serialised the body
# would change it, and the signature would no longer match.
BODY = '{"ref":  "refs/heads/main", "after":"abc123",\n "note": "café"}'
failed = 0


def sign(key):
    return hmac.new(key.encode(), BODY.encode(), hashlib.sha256).hexdigest()


def deliver(name, status, relayed, signature, env=None, path="/hook", method="POST", encoded=False, extra=None, check=lambda s: True):
    global failed
    open(log, "w").close()
    os.environ.pop("WEBHOOK_SECRET", None)
    os.environ["HATCHET_WEBHOOK_URL"] = hatchet + path
    os.environ.update(env or {})
    event = {
        "requestContext": {"http": {"method": method}},
        # API Gateway and function URLs differ in how they capitalise headers.
        "headers": {"Content-Type": "application/json", "X-Forgejo-Event": "push", "x-forgejo-signature": signature, **(extra or {})},
        "body": base64.b64encode(BODY.encode()).decode() if encoded else BODY,
        "isBase64Encoded": encoded,
    }
    got = handler.handler(event)
    seen = [json.loads(line) for line in open(log).read().splitlines() if line]
    ok = got["statusCode"] == status and len(seen) == relayed and (relayed == 0 or check(seen[0]))
    print(f"  {'ok  ' if ok else 'FAIL'} {name}: HTTP {got['statusCode']}, relayed {len(seen)}")
    if not ok:
        failed += 1
        print("       ", seen)


unchanged = lambda s: s["ok"] and s["body"] == BODY and s["event"] == "push" and s["content_type"] == "application/json" and not s["leaked"]
deliver("a signed delivery is relayed unchanged", 202, 1, sign(secret), extra={"Cookie": "session=1", "Authorization": "Bearer x"}, check=unchanged)
deliver("a base64-encoded body is relayed as the bytes it stands for", 202, 1, sign(secret), encoded=True, check=unchanged)
deliver("GitHub's form of signature is relayed too", 202, 1, "", extra={"X-Hub-Signature-256": "sha256=" + sign(secret), "X-GitHub-Event": "push"}, check=lambda s: s["ok"])
deliver("without the secret, a forged one is relayed and Hatchet refuses it", 403, 1, sign("wrong"), check=lambda s: not s["ok"])
deliver("with the secret, a forged one is turned away here", 401, 0, sign("wrong"), env={"WEBHOOK_SECRET": secret})
deliver("with the secret, an unsigned one is turned away here", 401, 0, "", env={"WEBHOOK_SECRET": secret})
deliver("with the secret, a signed one still goes through", 202, 1, sign(secret), env={"WEBHOOK_SECRET": secret}, check=lambda s: s["ok"])
deliver("Hatchet failing is passed back, to be retried", 500, 0, sign(secret), path="/broken")
deliver("Hatchet unreachable is a failure, to be retried", 502, 0, sign(secret), env={"HATCHET_WEBHOOK_URL": "http://127.0.0.1:9/"})
deliver("anything but POST is refused", 405, 0, sign(secret), method="GET")

sys.exit(1 if failed else 0)
