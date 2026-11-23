#!/usr/bin/env node
/**
 * Vercel Preview Review — sends Vercel Toolbar comments to the issue's agent.
 *
 * Two ways in, one way out:
 *   - A poller (needs MULTICA_PLUGIN_TOKEN) watches the team's open toolbar
 *     threads. Once a preview branch has had no new comment for the quiet
 *     window, the new comments go to every issue linked to that branch's open
 *     pull request.
 *   - The "Send preview review to agent" issue action sends right away.
 * Either way the review is one comment that wakes the issue's agent. A hidden
 * marker in it lists the Vercel messages it carried, so nothing is sent twice.
 *
 * Run (see ../README.md for the Docker overlay):
 *   VERCEL_TOKEN=... VERCEL_TEAM_ID=team_... MULTICA_API_URL=http://backend:8080 \
 *   MULTICA_PLUGIN_TOKEN=mpi_... MULTICA_SIGNING_SECRET=whsec_... node handler.mjs
 */

import { createServer } from "node:https";
import { createHmac, timingSafeEqual } from "node:crypto";
import { readFileSync } from "node:fs";

const env = process.env;
const PORT = Number(env.PORT ?? 8787);
const SIGNING_SECRET = env.MULTICA_SIGNING_SECRET ?? "";
const PLUGIN_TOKEN = env.MULTICA_PLUGIN_TOKEN ?? "";
const VERCEL_TOKEN = env.VERCEL_TOKEN ?? "";
const VERCEL_TEAM_ID = env.VERCEL_TEAM_ID ?? "";
const MULTICA_API_URL = (env.MULTICA_API_URL ?? "").replace(/\/+$/, "");
const POLL_MS = Number(env.POLL_SECONDS ?? 30) * 1000;
const QUIET_MS = Number(env.QUIET_SECONDS ?? 120) * 1000;
const TLS_DIR = env.TLS_DIR ?? "/tls";
const HOOK_KEY = "send_preview_review";
const TOLERANCE_SECONDS = 5 * 60;
// Multica rejects comments over 64 KiB; leave room for the header.
const MAX_COMMENT_CHARS = 60_000;
const MARKER = /<!-- vercel-preview-review messages: ([^>]*?) -->/g;

for (const [name, value] of Object.entries({ VERCEL_TOKEN, VERCEL_TEAM_ID })) {
  if (!value) {
    console.error(`${name} is required.`);
    process.exit(1);
  }
}

class HttpError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

// --- Signature check (same contract as examples/plugins/triage-notify) ---

const seenSignatures = new Map();

function rememberSignature(signature, now) {
  for (const [seen, at] of seenSignatures) {
    if (now - at > TOLERANCE_SECONDS * 1000) seenSignatures.delete(seen);
  }
  if (seenSignatures.has(signature)) return false;
  seenSignatures.set(signature, now);
  return true;
}

function verify(rawBody, headers) {
  // The secret only exists once the plugin is installed, so the service starts
  // without it and refuses hooks until it is configured.
  if (!SIGNING_SECRET) return "MULTICA_SIGNING_SECRET is not configured";
  const timestamp = headers["x-multica-timestamp"];
  const presented = String(headers["x-multica-signature"] ?? "").replace(/^v1=/, "");
  if (!timestamp || !presented) return "missing signature headers";

  const drift = Math.abs(Math.floor(Date.now() / 1000) - Number(timestamp));
  if (!Number.isFinite(drift) || drift > TOLERANCE_SECONDS) return "timestamp outside the accepted window";

  const secret = Buffer.from(SIGNING_SECRET.replace(/^whsec_/, ""), "hex");
  const expected = createHmac("sha256", secret).update(timestamp).update(".").update(rawBody).digest("hex");
  const expectedBytes = Buffer.from(expected, "utf8");
  const presentedBytes = Buffer.from(presented, "utf8");
  if (expectedBytes.length !== presentedBytes.length || !timingSafeEqual(expectedBytes, presentedBytes)) {
    return "signature does not match";
  }
  if (!rememberSignature(presented, Date.now())) return "this request was already delivered";
  return null;
}

// --- Multica ---

// A hook call answers with its callback token; the poller uses the install token.
function hookAuth(body) {
  // Inside Docker the public callback URL may not route back to the backend,
  // so keep its versioned path and swap in the internal origin.
  const path = body.callback_url ? new URL(body.callback_url).pathname.replace(/\/+$/, "") : "/v1";
  const base = MULTICA_API_URL ? MULTICA_API_URL + path : body.callback_url;
  if (!base) throw new HttpError(502, "Multica sent no callback_url; set MULTICA_API_URL");
  return { base, token: body.callback_token };
}

async function multica(auth, method, path, payload) {
  const response = await fetch(auth.base + path, {
    method,
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${auth.token}` },
    body: payload === undefined ? undefined : JSON.stringify(payload),
  });
  if (!response.ok) throw new Error(`Multica ${method} ${path} answered ${response.status}: ${await response.text()}`);
  return response.json();
}

const isActive = (pr) => pr.branch && (pr.state === "open" || pr.state === "draft");

// --- Vercel ---

async function vercel(path, query = {}) {
  const url = new URL(path, "https://api.vercel.com");
  url.searchParams.set("teamId", VERCEL_TEAM_ID);
  for (const [key, value] of Object.entries(query)) url.searchParams.set(key, String(value));
  const response = await fetch(url, { headers: { Authorization: `Bearer ${VERCEL_TOKEN}` } });
  if (!response.ok) throw new Error(`Vercel ${url.pathname} answered ${response.status}`);
  return response.json();
}

async function openThreads(query = {}) {
  const threads = [];
  // A page can come back empty and still name a next one, so follow `next`
  // instead of stopping on the first empty page.
  for (let offset = 0, pages = 0; offset !== undefined && pages < 20; pages++) {
    const page = await vercel("/v1/toolbar/threads", { ...query, status: "unresolved", limit: 100, offset });
    threads.push(...(page.threads ?? []));
    offset = page.pagination?.next;
  }
  return threads.filter((thread) => !thread.resolved);
}

const projectRepos = new Map();

// Branch names repeat across projects ("main"), so a thread only counts when
// its Vercel project deploys the pull request's repository.
async function projectRepo(projectId) {
  if (!projectRepos.has(projectId)) {
    const { link } = await vercel(`/v9/projects/${encodeURIComponent(projectId)}`);
    const owner = link?.org ?? link?.projectNamespace ?? link?.owner;
    const name = link?.repo ?? link?.projectName ?? link?.slug;
    projectRepos.set(projectId, owner && name ? { owner, name } : null);
  }
  return projectRepos.get(projectId);
}

const sameRepo = (repo, pr) =>
  repo && `${repo.owner}/${repo.name}`.toLowerCase() === `${pr.repo_owner}/${pr.repo_name}`.toLowerCase();

const messageIds = (thread) => (thread.messages ?? []).map((message) => message.id);
const lastActivity = (thread) => Math.max(0, ...(thread.messages ?? []).map((message) => message.timestamp ?? 0));

// --- Comment ---

const oneLine = (text) => String(text ?? "").replace(/\s+/g, " ").trim();

// The React tree Vercel captures runs root -> selected element; the last few
// lines are the components that actually render the commented element.
function componentPath(tree) {
  const lines = tree.split("\n").filter((line) => line.trim());
  const selected = lines.find((line) => line.startsWith("Selected:"));
  const tail = lines.slice(-6);
  const indent = Math.min(...tail.map((line) => line.match(/^ */)[0].length));
  return [selected, ...tail.map((line) => line.slice(indent))].filter(Boolean).map((line) => line.slice(0, 200));
}

function images(message) {
  return (message?.attachments ?? [])
    .filter((attachment) => attachment.mimeType?.startsWith("image/"))
    .map((attachment) => `![${attachment.filename ?? "screenshot"}](${attachment.url})`);
}

function renderThread({ thread }, index) {
  const [first, ...replies] = thread.messages ?? [];
  const context = thread.context ?? {};
  const device = context.device ?? {};
  const page = context.href ? `[${context.path ?? context.href}](${context.href})` : (context.path ?? "unknown page");
  const viewport = device.screenWidth
    ? ` · ${device.screenWidth}×${device.screenHeight} @${device.devicePixelRatio ?? 1}x`
    : "";
  const lines = [`### ${index + 1}. ${oneLine(first?.text) || "(screenshot only)"}`, "", `- **Page:** ${page}${viewport}`];
  if (context.selector) lines.push(`- **Element:** \`${context.selector}\``);
  lines.push(`- **Vercel thread:** [\`${thread.id}\`](${thread.webUrl})`);
  const shots = images(first);
  if (shots.length) lines.push("", ...shots);
  if (context.frameworkContext) lines.push("", "```text", ...componentPath(context.frameworkContext), "```");
  for (const reply of replies) {
    const author = reply.author?.name ?? reply.author?.username ?? "reply";
    lines.push("", `> **${author}:** ${oneLine(reply.text)}`, ...images(reply).map((image) => `> ${image}`));
  }
  return lines.join("\n");
}

// Returns the comment and how many threads did not fit.
function renderReview(items) {
  const prs = [...new Map(items.map(({ pr }) => [pr.id, pr])).values()];
  const authors = [
    ...new Set(items.flatMap(({ thread }) => (thread.messages ?? []).map((m) => m.author?.name ?? m.author?.username))),
  ].filter(Boolean);
  const header = [
    `Preview review: ${items.length} open comment${items.length === 1 ? "" : "s"} on ` +
      prs.map((pr) => `[${pr.repo_name}#${pr.number}](${pr.html_url}) (\`${pr.branch}\`)`).join(", ") +
      (authors.length ? ` from ${authors.join(", ")}` : ""),
    "",
    "Fix each item in that pull request, then close its Vercel thread with " +
      '`vercel comments resolve <thread> --message "<what changed>"`.',
  ].join("\n");

  let content = header;
  const sent = [];
  for (const [index, item] of items.entries()) {
    const section = `\n\n---\n\n${renderThread(item, index)}`;
    if (content.length + section.length > MAX_COMMENT_CHARS) {
      content += `\n\n---\n\n…and ${items.length - index} more open comments on Vercel, sent next.`;
      break;
    }
    content += section;
    sent.push(item);
  }
  content += `\n\n<!-- vercel-preview-review messages: ${sent.flatMap(({ thread }) => messageIds(thread)).join(" ")} -->`;
  return { content, sent, remaining: items.length - sent.length };
}

// Sends are serialized so the poller and the issue action cannot both post
// the same messages to one issue.
let sending = Promise.resolve();
function exclusive(task) {
  const run = sending.then(task, task);
  sending = run.catch(() => {});
  return run;
}

// Posts the threads the issue has not been sent yet; null when there are none.
function sendReview(auth, issueId, items) {
  return exclusive(async () => {
    const issuePath = `/issues/${encodeURIComponent(issueId)}`;
    const { comments = [] } = await multica(auth, "GET", `${issuePath}/comments`);
    const sent = new Set(comments.flatMap((comment) => [...comment.content.matchAll(MARKER)].flatMap((m) => m[1].split(" "))));
    const fresh = items
      .filter(({ thread }) => messageIds(thread).some((id) => !sent.has(id)))
      .sort((a, b) => (a.thread.messages?.[0]?.timestamp ?? 0) - (b.thread.messages?.[0]?.timestamp ?? 0));
    if (!fresh.length) return null;

    const review = renderReview(fresh);
    const comment = await multica(auth, "POST", `${issuePath}/comments`, { content: review.content, trigger_agents: true });
    return { comment_id: comment.id, threads: review.sent.map(({ thread }) => thread.id), remaining: review.remaining };
  });
}

// --- The issue action ---

async function sendPreviewReviewNow(body) {
  if (!body.issue_id) throw new HttpError(422, "this action runs on an issue");
  const auth = hookAuth(body);
  const { pull_requests: linked = [] } = await multica(auth, "GET", `/issues/${encodeURIComponent(body.issue_id)}/pull-requests`);
  const active = linked.filter(isActive);
  const pullRequests = active.length ? active : linked.filter((pr) => pr.branch);
  if (!pullRequests.length) throw new HttpError(422, "the issue has no linked pull request with a branch");

  const items = [];
  const seen = new Set();
  for (const pr of pullRequests) {
    for (const thread of await openThreads({ branch: pr.branch })) {
      if (seen.has(thread.id) || !sameRepo(await projectRepo(thread.projectId), pr)) continue;
      seen.add(thread.id);
      items.push({ pr, thread });
    }
  }
  const result = items.length ? await sendReview(auth, body.issue_id, items) : null;
  if (!result) throw new HttpError(404, "no new Vercel preview comments on the linked branches");
  return result;
}

// --- The poller ---

// Branches whose current comments were already handled, keyed by project and
// branch, so an idle review costs one Vercel call per tick and nothing else.
const handled = new Map();

// True once a branch's comments need no more work: its project deploys no
// known repository, or every issue linked to its open pull request has them.
// False while no open pull request is linked, so a link made later still
// picks the comments up.
async function sendBranch(auth, threads) {
  const { projectId, branch } = threads[0];
  const repo = await projectRepo(projectId);
  if (!repo) return true;
  const query = new URLSearchParams({ repo_owner: repo.owner, repo_name: repo.name, branch });
  const { pull_requests: linked = [] } = await multica(auth, "GET", `/pull-requests?${query}`);
  const active = linked.filter(isActive);
  let done = active.length > 0;
  for (const pr of active) {
    for (const issueId of pr.issue_ids) {
      const result = await sendReview(auth, issueId, threads.map((thread) => ({ pr, thread })));
      if (!result) continue;
      console.log(`${repo.owner}/${repo.name}@${branch}: sent ${result.threads.length} comments to issue ${issueId}`);
      if (result.remaining) done = false;
    }
  }
  return done;
}

async function pollOnce() {
  const auth = { base: `${MULTICA_API_URL}/v1`, token: PLUGIN_TOKEN };
  const branches = new Map();
  for (const thread of await openThreads()) {
    const key = `${thread.projectId} ${thread.branch}`;
    if (!branches.has(key)) branches.set(key, []);
    branches.get(key).push(thread);
  }
  for (const key of handled.keys()) if (!branches.has(key)) handled.delete(key);

  for (const [key, threads] of branches) {
    const fingerprint = threads.flatMap(messageIds).sort().join(" ");
    if (handled.get(key) === fingerprint) continue;
    // Still reviewing: wait until the newest comment has gone quiet.
    if (Date.now() - Math.max(...threads.map(lastActivity)) < QUIET_MS) continue;
    try {
      if (await sendBranch(auth, threads)) handled.set(key, fingerprint);
    } catch (error) {
      console.error(`${key}: ${error.message}`);
    }
  }
}

function startPoller() {
  if (!PLUGIN_TOKEN || !MULTICA_API_URL) {
    console.log("automatic sending is off: set MULTICA_PLUGIN_TOKEN and MULTICA_API_URL");
    return;
  }
  console.log(`sending reviews automatically ${QUIET_MS / 1000}s after the last comment`);
  const tick = async () => {
    try {
      await pollOnce();
    } catch (error) {
      console.error(`poll failed: ${error.message}`);
    }
    setTimeout(tick, POLL_MS);
  };
  tick();
}

// --- Server ---

function reply(response, status, payload) {
  response.writeHead(status, { "Content-Type": "application/json" });
  response.end(JSON.stringify(payload));
}

const server = createServer(
  { cert: readFileSync(`${TLS_DIR}/cert.pem`), key: readFileSync(`${TLS_DIR}/key.pem`) },
  (request, response) => {
    if (request.method !== "POST" || request.url !== "/hooks/preview-review") {
      response.writeHead(404).end();
      return;
    }
    const chunks = [];
    request.on("data", (chunk) => chunks.push(chunk));
    request.on("end", async () => {
      const rawBody = Buffer.concat(chunks);
      const failure = verify(rawBody, request.headers);
      if (failure) {
        console.warn(`rejected: ${failure}`);
        reply(response, 401, { error: failure });
        return;
      }

      const body = JSON.parse(rawBody.toString("utf8"));
      if (body.hook_key !== HOOK_KEY) {
        reply(response, 404, { error: `unknown hook ${body.hook_key}` });
        return;
      }
      try {
        const result = await sendPreviewReviewNow(body);
        console.log(`issue ${body.issue_id}: sent ${result.threads.length} comments on request`);
        reply(response, 200, result);
      } catch (error) {
        const status = error instanceof HttpError ? error.status : 502;
        console.error(`issue ${body.issue_id}: ${error.message}`);
        reply(response, status, { error: status === 502 ? "preview review failed" : error.message });
      }
    });
  },
);

server.listen(PORT, () => {
  console.log(`preview review hook listening on https://0.0.0.0:${PORT}`);
  startPoller();
});
