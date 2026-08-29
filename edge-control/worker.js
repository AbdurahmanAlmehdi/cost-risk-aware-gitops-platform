// A power switch for the review host, reachable from a phone.
//
// The host bills by the hour, so it lives switched off. Turning it on previously meant an
// AWS console login, which needs a passkey that lives on one laptop -- so in practice the
// person with the laptop was a bottleneck for everyone else wanting to look at the
// platform.
//
// This runs on Cloudflare rather than on the host itself, which is the whole point: the
// demonstration dashboard cannot start the machine it runs on. A Worker is up whether or
// not the instance is, so it can turn the instance on as well as off.
//
// Authentication is Cloudflare Access, the same allow-list that gates the three
// demonstration hostnames. Access terminates in front of this Worker, so a request that
// arrives here has already been authenticated; the identity is read from the header
// Access sets, purely so the log records who pressed the button.
//
// The AWS credentials are Worker secrets, belonging to an IAM user that can do nothing but
// start, stop and describe this one instance. They are never in this file.

const REGION = "eu-central-1";
const SERVICE = "ec2";
const HOST = `ec2.${REGION}.amazonaws.com`;
const API_VERSION = "2016-11-15";

const enc = new TextEncoder();

const hex = (buf) =>
  [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, "0")).join("");

const sha256 = async (str) => hex(await crypto.subtle.digest("SHA-256", enc.encode(str)));

async function hmac(key, msg) {
  const k = await crypto.subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, [
    "sign",
  ]);
  return crypto.subtle.sign("HMAC", k, enc.encode(msg));
}

// Signature Version 4, by hand.
//
// Hand-rolled rather than pulled from a library because the Worker is deployed straight
// from a single file with no build step: adding a bundler to sign three request types
// would be more moving parts than the signing itself.
async function signedFetch(action, params, env) {
  const body = new URLSearchParams({ Action: action, Version: API_VERSION, ...params }).toString();

  const now = new Date();
  const amzDate = now.toISOString().replace(/[:-]|\.\d{3}/g, "");
  const dateStamp = amzDate.slice(0, 8);

  const payloadHash = await sha256(body);
  const canonicalHeaders =
    `content-type:application/x-www-form-urlencoded; charset=utf-8\n` +
    `host:${HOST}\n` +
    `x-amz-date:${amzDate}\n`;
  const signedHeaders = "content-type;host;x-amz-date";

  const canonicalRequest = [
    "POST",
    "/",
    "",
    canonicalHeaders,
    signedHeaders,
    payloadHash,
  ].join("\n");

  const scope = `${dateStamp}/${REGION}/${SERVICE}/aws4_request`;
  const stringToSign = [
    "AWS4-HMAC-SHA256",
    amzDate,
    scope,
    await sha256(canonicalRequest),
  ].join("\n");

  let key = enc.encode(`AWS4${env.AWS_SECRET_ACCESS_KEY}`);
  for (const part of [dateStamp, REGION, SERVICE, "aws4_request"]) {
    key = await hmac(key, part);
  }
  const signature = hex(await hmac(key, stringToSign));

  const authorization =
    `AWS4-HMAC-SHA256 Credential=${env.AWS_ACCESS_KEY_ID}/${scope}, ` +
    `SignedHeaders=${signedHeaders}, Signature=${signature}`;

  const res = await fetch(`https://${HOST}/`, {
    method: "POST",
    headers: {
      "content-type": "application/x-www-form-urlencoded; charset=utf-8",
      "x-amz-date": amzDate,
      authorization: authorization,
    },
    body: body,
  });

  return { ok: res.ok, status: res.status, text: await res.text() };
}

// EC2's query API answers in XML, and one tag each is all this needs.
const tag = (xml, name) => {
  const m = xml.match(new RegExp(`<${name}>([^<]*)</${name}>`));
  return m ? m[1] : null;
};

async function describe(env) {
  const r = await signedFetch("DescribeInstances", { "InstanceId.1": env.INSTANCE_ID }, env);
  if (!r.ok) return { error: tag(r.text, "Message") || `AWS returned ${r.status}` };
  return {
    state: tag(r.text, "name") || "unknown",
    address: tag(r.text, "ipAddress"),
  };
}

async function power(action, env) {
  const r = await signedFetch(action, { "InstanceId.1": env.INSTANCE_ID }, env);
  if (!r.ok) return { error: tag(r.text, "Message") || `AWS returned ${r.status}` };
  return { ok: true };
}

const PAGE = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Review host</title>
<style>
  :root {
    --paper: #f6f7f9; --surface: #fff; --ink: #0f141b; --soft: #5b6472;
    --rule: #dce0e7; --on: #2c6b68; --off: #6b7484; --warn: #8f6320; --bad: #a03a2e;
    --sans: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    --mono: ui-monospace, SFMono-Regular, Menlo, monospace;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --paper: #0f141b; --surface: #161c25; --ink: #e8ebf0; --soft: #98a1b0;
      --rule: #29323f; --on: #6fb8b2; --off: #838c9c; --warn: #d6a44e; --bad: #e08376;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; background: var(--paper); color: var(--ink);
    font-family: var(--sans); display: grid; place-items: center; padding: 24px;
  }
  main { width: 100%; max-width: 380px; }
  h1 { font-size: 1.05rem; font-weight: 650; margin: 0 0 4px; }
  .sub { color: var(--soft); font-size: .85rem; margin: 0 0 24px; }
  .card {
    background: var(--surface); border: 1px solid var(--rule); border-radius: 10px;
    padding: 22px; margin-bottom: 16px;
  }
  .state { display: flex; align-items: center; gap: 10px; margin-bottom: 6px; }
  .dot { width: 10px; height: 10px; border-radius: 50%; background: var(--off); flex: none; }
  .dot.running { background: var(--on); }
  .dot.pending, .dot.stopping { background: var(--warn); }
  .label { font-family: var(--mono); font-size: 1.25rem; letter-spacing: -.01em; }
  .addr { font-family: var(--mono); font-size: .8rem; color: var(--soft); word-break: break-all; }
  .cost { color: var(--soft); font-size: .8rem; margin-top: 14px; }
  .actions { display: grid; gap: 10px; }
  button {
    font: inherit; font-weight: 600; padding: 14px; border-radius: 8px; cursor: pointer;
    border: 1px solid var(--rule); background: var(--surface); color: var(--ink);
    -webkit-tap-highlight-color: transparent;
  }
  button.primary { background: var(--ink); color: var(--paper); border-color: var(--ink); }
  button:disabled { opacity: .4; cursor: not-allowed; }
  button:focus-visible { outline: 2px solid var(--on); outline-offset: 2px; }
  .msg { font-size: .85rem; margin-top: 14px; min-height: 1.2em; }
  .msg.err { color: var(--bad); }
  .links { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 4px; }
  .links a {
    font-family: var(--mono); font-size: .75rem; color: var(--soft);
    text-decoration: none; border: 1px solid var(--rule); border-radius: 999px;
    padding: 5px 11px;
  }
  .who { color: var(--soft); font-size: .72rem; margin-top: 20px; text-align: center; }
</style>
</head>
<body>
<main>
  <h1>Review host</h1>
  <p class="sub">Cost- and Risk-Aware GitOps Platform</p>

  <div class="card">
    <div class="state">
      <span class="dot" id="dot"></span>
      <span class="label" id="label">checking</span>
    </div>
    <div class="addr" id="addr"></div>
    <p class="cost" id="cost"></p>
  </div>

  <div class="card">
    <div class="actions">
      <button class="primary" id="start">Start</button>
      <button id="stop">Stop</button>
    </div>
    <p class="msg" id="msg"></p>
  </div>

  <div class="links">
    <a href="https://gitops.abdurahman.ly">gitops</a>
    <a href="https://argocd.abdurahman.ly">argocd</a>
    <a href="https://grafana.abdurahman.ly">grafana</a>
  </div>

  <p class="who" id="who"></p>
</main>
<script>
  const $ = (id) => document.getElementById(id);
  let busy = false;

  function render(s) {
    if (s.error) {
      $("label").textContent = "unavailable";
      $("msg").textContent = s.error;
      $("msg").className = "msg err";
      return;
    }
    $("dot").className = "dot " + s.state;
    $("label").textContent = s.state;
    $("addr").textContent = s.address ? s.address : "";
    $("cost").textContent =
      s.state === "running"
        ? "Billing at $0.2415/hour. Stops itself after 90 minutes unused."
        : "Not billing for compute. The disk costs about $3.50/month.";
    $("start").disabled = busy || s.state !== "stopped";
    $("stop").disabled = busy || s.state !== "running";
    if (s.identity) $("who").textContent = "signed in as " + s.identity;
  }

  async function refresh() {
    try {
      render(await (await fetch("/api/state")).json());
    } catch (e) {
      render({ error: "Could not reach the control plane." });
    }
  }

  async function act(what) {
    busy = true;
    $("msg").className = "msg";
    $("msg").textContent = what === "start" ? "Starting..." : "Stopping...";
    $("start").disabled = $("stop").disabled = true;
    try {
      const r = await (await fetch("/api/" + what, { method: "POST" })).json();
      if (r.error) {
        $("msg").textContent = r.error;
        $("msg").className = "msg err";
      } else {
        $("msg").textContent =
          what === "start"
            ? "Starting. It takes about a minute to answer."
            : "Stopping.";
      }
    } catch (e) {
      $("msg").textContent = "Request failed.";
      $("msg").className = "msg err";
    }
    busy = false;
    // Poll while the instance moves through its pending or stopping state, then settle.
    for (let i = 0; i < 20; i++) {
      await new Promise((r) => setTimeout(r, 3000));
      await refresh();
      const s = $("label").textContent;
      if (s === "running" || s === "stopped") break;
    }
  }

  $("start").onclick = () => act("start");
  $("stop").onclick = () => act("stop");
  refresh();
  setInterval(() => { if (!busy) refresh(); }, 15000);
</script>
</body>
</html>`;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);

    // Access puts the authenticated address in this header. It is read for the log line
    // and the footer only -- the authorisation decision was already made at the edge, and
    // re-deciding it here from a header would be weaker, not stronger.
    const identity = request.headers.get("cf-access-authenticated-user-email") || "";

    if (!env.AWS_ACCESS_KEY_ID || !env.AWS_SECRET_ACCESS_KEY || !env.INSTANCE_ID) {
      const missing = [
        !env.AWS_ACCESS_KEY_ID && "AWS_ACCESS_KEY_ID",
        !env.AWS_SECRET_ACCESS_KEY && "AWS_SECRET_ACCESS_KEY",
        !env.INSTANCE_ID && "INSTANCE_ID",
      ].filter(Boolean);
      const msg = `Not configured yet. Missing: ${missing.join(", ")}.`;
      if (url.pathname.startsWith("/api/")) {
        return Response.json({ error: msg }, { status: 503 });
      }
      return new Response(PAGE, { headers: { "content-type": "text/html; charset=utf-8" } });
    }

    if (url.pathname === "/api/state") {
      const s = await describe(env);
      return Response.json({ ...s, identity: identity });
    }

    if (request.method === "POST" && (url.pathname === "/api/start" || url.pathname === "/api/stop")) {
      const action = url.pathname === "/api/start" ? "StartInstances" : "StopInstances";
      console.log(`${action} requested by ${identity || "unknown"}`);
      return Response.json(await power(action, env));
    }

    return new Response(PAGE, { headers: { "content-type": "text/html; charset=utf-8" } });
  },
};
