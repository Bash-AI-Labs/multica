#!/usr/bin/env node
/**
 * Vercel Preview Review — the hook server behind "Send preview review to agent".
 *
 * Multica calls this when someone picks the action on an issue. It reads the
 * issue's linked pull requests, collects the open Vercel Toolbar comments left
 * on those branches' preview deployments, and posts them back as one comment
 * that wakes the issue's agent.
 *
 * Run (see ../README.md for the Docker overlay):
 *   MULTICA_SIGNING_SECRET=whsec_... VERCEL_TOKEN=... VERCEL_TEAM_ID=team_... node handler.mjs
 */

import { createServer } from "node:https";
import { createHmac, timingSafeEqual } from "node:crypto";
import { readFileSync } from "node:fs";

const env = process.env;
const PORT = Number(env.PORT ?? 8787);
const SIGNING_SECRET = env.MULTICA_SIGNING_SECRET ?? "";
const VERCEL_TOKEN = env.VERCEL_TOKEN ?? "";
const VERCEL_TEAM_ID = env.VERCEL_TEAM_ID ?? "";
const MULTICA_API_URL = (env.MULTICA_API_URL ?? "").replace(/\/+$/, "");
const TLS_DIR = env.TLS_DIR ?? "/tls";
const HOOK_KEY = "send_preview_review";
const TOLERANCE_SECONDS = 5 * 60;
// Multica rejects comments over 64 KiB; leave room for the header.
const MAX_COMMENT_CHARS = 60_000;

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

// --- Multica callbacks ---

function multicaBase(body) {
  if (!MULTICA_API_URL) return body.callback_url;
  // Inside Docker the public callback URL may not route back to the backend,
  // so keep its versioned path and swap in the internal origin.
  const path = body.callback_url ? new URL(body.callback_url).pathname : "/v1";
  return MULTICA_API_URL + path.replace(/\/+$/, "");
}

async function multica(body, method, path, payload) {
  const base = multicaBase(body);
  if (!base) throw new HttpError(502, "Multica sent no callback_url; set MULTICA_API_URL");
  const response = await fetch(base + path, {
    method,
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${body.callback_token}` },
    body: payload === undefined ? undefined : JSON.stringify(payload),
  });
  if (!response.ok) throw new Error(`Multica ${method} ${path} answered ${response.status}: ${await response.text()}`);
  return response.json();
}

// --- Vercel ---

async function vercel(path, query = {}) {
  const url = new URL(path, "https://api.vercel.com");
  url.searchParams.set("teamId", VERCEL_TEAM_ID);
  for (const [key, value] of Object.entries(query)) url.searchParams.set(key, String(value));
  const response = await fetch(url, { headers: { Authorization: `Bearer ${VERCEL_TOKEN}` } });
  if (!response.ok) throw new Error(`Vercel ${url.pathname} answered ${response.status}`);
  return response.json();
}

async function openThreads(branch) {
  const threads = [];
  // A page can come back empty and still name a next one, so follow `next`
  // instead of stopping on the first empty page.
  for (let offset = 0, pages = 0; offset !== undefined && pages < 20; pages++) {
    const page = await vercel("/v1/toolbar/threads", { branch, limit: 100, offset });
    threads.push(...(page.threads ?? []));
    offset = page.pagination?.next;
  }
  return threads;
}

const projectRepos = new Map();

// Branch names repeat across projects ("main"), so a thread only counts when
// its Vercel project deploys the pull request's repository.
async function projectRepo(projectId) {
  if (!projectRepos.has(projectId)) {
    const { link } = await vercel(`/v9/projects/${encodeURIComponent(projectId)}`);
    const owner = link?.org ?? link?.projectNamespace ?? link?.owner;
    const repo = link?.repo ?? link?.projectName ?? link?.slug;
    projectRepos.set(projectId, owner && repo ? `${owner}/${repo}`.toLowerCase() : null);
  }
  return projectRepos.get(projectId);
}

async function collectReview(pullRequests) {
  const seen = new Set();
  const items = [];
  for (const pr of pullRequests) {
    const repo = `${pr.repo_owner}/${pr.repo_name}`.toLowerCase();
    for (const thread of await openThreads(pr.branch)) {
      if (thread.resolved || seen.has(thread.id)) continue;
      if ((await projectRepo(thread.projectId)) !== repo) continue;
      seen.add(thread.id);
      items.push({ pr, thread });
    }
  }
  const startedAt = ({ thread }) => thread.messages?.[0]?.timestamp ?? 0;
  return items.sort((a, b) => startedAt(a) - startedAt(b));
}

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

function renderReview(items) {
  const prs = [...new Map(items.map(({ pr }) => [pr.id, pr])).values()];
  const header = [
    `Preview review: ${items.length} open comment${items.length === 1 ? "" : "s"} on ` +
      prs.map((pr) => `[${pr.repo_name}#${pr.number}](${pr.html_url}) (\`${pr.branch}\`)`).join(", "),
    "",
    "Fix each item in that pull request, then close its Vercel thread with " +
      '`vercel comments resolve <thread> --message "<what changed>"`.',
  ].join("\n");

  let content = header;
  for (const [index, item] of items.entries()) {
    const section = `\n\n---\n\n${renderThread(item, index)}`;
    if (content.length + section.length > MAX_COMMENT_CHARS) {
      return `${content}\n\n---\n\n…and ${items.length - index} more open comments on Vercel.`;
    }
    content += section;
  }
  return content;
}

// --- Hook ---

async function sendPreviewReview(body) {
  if (!body.issue_id) throw new HttpError(422, "this action runs on an issue");
  const issuePath = `/issues/${encodeURIComponent(body.issue_id)}`;

  const { pull_requests: linked = [] } = await multica(body, "GET", `${issuePath}/pull-requests`);
  const withBranch = linked.filter((pr) => pr.branch);
  const active = withBranch.filter((pr) => pr.state === "open" || pr.state === "draft");
  const pullRequests = active.length ? active : withBranch;
  if (!pullRequests.length) throw new HttpError(422, "the issue has no linked pull request with a branch");

  const items = await collectReview(pullRequests);
  if (!items.length) throw new HttpError(404, "no open Vercel preview comments on the linked branches");

  const comment = await multica(body, "POST", `${issuePath}/comments`, {
    content: renderReview(items),
    trigger_agents: true,
  });
  return { comment_id: comment.id, threads: items.map(({ thread }) => thread.id) };
}

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
        const result = await sendPreviewReview(body);
        console.log(`issue ${body.issue_id}: sent ${result.threads.length} preview comments`);
        reply(response, 200, result);
      } catch (error) {
        const status = error instanceof HttpError ? error.status : 502;
        console.error(`issue ${body.issue_id}: ${error.message}`);
        reply(response, status, { error: status === 502 ? "preview review failed" : error.message });
      }
    });
  },
);

server.listen(PORT, () => console.log(`preview review hook listening on https://0.0.0.0:${PORT}`));
