// A door for starci, as a Cloudflare Worker.
//
// It relays a forge's webhook delivery to Hatchet's webhook address, and that
// is all. Hatchet checks the signature and raises the event; a starci worker
// does the rest. See ../README.md for the contract.
//
// Settings (wrangler secrets or vars):
//   HATCHET_WEBHOOK_URL  where to relay to, as printed by `starci hooks add`
//   WEBHOOK_SECRET       optional. With it, deliveries that are not signed by
//                        the forge are turned away here, before they cost
//                        Hatchet anything. Hatchet checks either way.

// The headers a delivery's meaning and proof travel in. Nothing else is
// passed on: not cookies, not the caller's address.
const RELAYED = [
  "content-type",
  "x-forgejo-event", "x-forgejo-signature",
  "x-gitea-event", "x-gitea-signature",
  "x-github-event", "x-hub-signature-256",
];
const SIGNATURES = ["x-forgejo-signature", "x-gitea-signature", "x-hub-signature-256"];

export default {
  async fetch(request, env) {
    if (request.method !== "POST") {
      return new Response("deliveries are POSTed here\n", { status: 405 });
    }
    // The signature is over the body exactly as sent, so it is kept as bytes.
    const body = await request.arrayBuffer();
    if (body.byteLength > 8 * 1024 * 1024) {
      return new Response("too large\n", { status: 413 });
    }

    if (env.WEBHOOK_SECRET && !(await signed(request.headers, body, env.WEBHOOK_SECRET))) {
      return new Response("the delivery is not signed with the shared secret\n", { status: 401 });
    }

    const headers = new Headers();
    for (const name of RELAYED) {
      const value = request.headers.get(name);
      if (value) headers.set(name, value);
    }
    let upstream;
    try {
      upstream = await fetch(env.HATCHET_WEBHOOK_URL, { method: "POST", headers, body });
    } catch (err) {
      // A failure the forge will see, and try again after.
      return new Response("could not reach hatchet\n", { status: 502 });
    }
    // Hatchet's answer is the forge's answer: 403 for a bad signature, a
    // server error for one worth retrying.
    return new Response(upstream.ok ? "accepted\n" : "refused\n", { status: upstream.ok ? 202 : upstream.status });
  },
};

async function signed(headers, body, secret) {
  let given = "";
  for (const name of SIGNATURES) {
    given = given || headers.get(name) || "";
  }
  given = given.replace(/^sha256=/, "");
  if (!/^[0-9a-f]{64}$/.test(given)) return false;

  const key = await crypto.subtle.importKey(
    "raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["verify"]);
  const bytes = new Uint8Array(given.match(/../g).map((pair) => parseInt(pair, 16)));
  // verify() compares in constant time.
  return crypto.subtle.verify("HMAC", key, bytes, body);
}
