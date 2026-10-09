import { createHmac } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import worker from "./worker.js";

const [hatchet, secret, log] = process.argv.slice(2);
// Odd spacing and a non-ASCII character: a door that re-serialised the body
// would change it, and the signature would no longer match.
const body = '{"ref":  "refs/heads/main", "after":"abc123",\n "note": "café"}';
const sign = (key) => createHmac("sha256", key).update(body).digest("hex");
let failed = 0;

async function deliver(name, { signature, env, path = "/hook", method = "POST", extra = {} }) {
  writeFileSync(log, "");
  const request = new Request("https://door.example/hooks/forgejo", {
    method,
    headers: { "content-type": "application/json", "x-forgejo-event": "push", "x-forgejo-signature": signature, ...extra },
    body: method === "POST" ? body : undefined,
  });
  const response = await worker.fetch(request, { HATCHET_WEBHOOK_URL: hatchet + path, ...env });
  const seen = readFileSync(log, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l));
  return { name, status: response.status, seen };
}

function expect(result, status, relayed, check = () => true) {
  const ok = result.status === status && result.seen.length === relayed && (relayed === 0 || check(result.seen[0]));
  console.log(`  ${ok ? "ok  " : "FAIL"} ${result.name}: HTTP ${result.status}, relayed ${result.seen.length}`);
  if (!ok) { failed++; console.log("       ", JSON.stringify(result.seen)); }
}

expect(await deliver("a signed delivery is relayed unchanged", { signature: sign(secret), extra: { cookie: "session=1", authorization: "Bearer x" } }),
  202, 1, (s) => s.ok && s.body === body && s.event === "push" && s.content_type === "application/json" && s.leaked.length === 0);
expect(await deliver("GitHub's form of signature is relayed too", { signature: "", extra: { "x-hub-signature-256": "sha256=" + sign(secret), "x-github-event": "push" } }),
  202, 1, (s) => s.ok);
expect(await deliver("without the secret, a forged one is relayed and Hatchet refuses it", { signature: sign("wrong") }),
  403, 1, (s) => !s.ok);
expect(await deliver("with the secret, a forged one is turned away here", { signature: sign("wrong"), env: { WEBHOOK_SECRET: secret } }), 401, 0);
expect(await deliver("with the secret, an unsigned one is turned away here", { signature: "", env: { WEBHOOK_SECRET: secret } }), 401, 0);
expect(await deliver("with the secret, a signed one still goes through", { signature: sign(secret), env: { WEBHOOK_SECRET: secret } }), 202, 1, (s) => s.ok);
expect(await deliver("Hatchet failing is passed back, to be retried", { signature: sign(secret), path: "/broken" }), 500, 0);
expect(await deliver("Hatchet unreachable is a failure, to be retried", { signature: sign(secret), env: { HATCHET_WEBHOOK_URL: "http://127.0.0.1:9/" } }), 502, 0);
expect(await deliver("anything but POST is refused", { signature: sign(secret), method: "GET" }), 405, 0);

process.exit(failed ? 1 : 0);
