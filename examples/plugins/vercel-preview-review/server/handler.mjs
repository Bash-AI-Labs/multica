#!/usr/bin/env node
/**
 * Vercel Preview Review — turns Vercel Toolbar comments into work for the agent
 * that owns the issue.
 *
 * Teammates review a pull request's Vercel preview and comment on it with the
 * Vercel Toolbar. Once a preview branch has had no new comment for the quiet
 * window, the poller posts the new comments to every issue linked to that
 * branch's open pull request, as one comment that wakes the issue's agent. The
 * "Send preview review to agent" issue action sends right away.
 *
 * The agent keeps its updates on the issue and never replies on Vercel: once an
 * item is done it only resolves the thread. A thread therefore carries nothing
 * but reviewers' words, and a reviewer who replies to it or reopens it sends it
 * back to the agent.
 *
 * What each issue has received is recorded per thread in the Plugin's workspace
 * storage, so nothing is sent twice: not across restarts, and not between the
 * poller and the issue action.
 *
 * Run (see ../README.md for the Docker overlay):
 *   VERCEL_TOKEN=... VERCEL_TEAM_ID=team_... MULTICA_API_URL=http://backend:8080 \
 *   MULTICA_PLUGIN_TOKEN=mpi_... MULTICA_SIGNING_SECRET=whsec_... node handler.mjs
 */

import { createServer } from "node:https";
import { createHmac, timingSafeEqual } from "node:crypto";
import { readFileSync } from "node:fs";
import { classify, deploymentBranch, record, renderReview } from "./review.mjs";

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
// A resolved thread's record is kept this long, so reopening it is recognised.
const CLOSED_RETENTION_MS = 30 * 24 * 60 * 60 * 1000;
const LEDGER_PREFIX = "thread-";

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

class UpstreamError extends Error {
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
  if (!response.ok) {
    throw new UpstreamError(response.status, `Multica ${method} ${path} answered ${response.status}: ${await response.text()}`);
  }
  return response.status === 204 ? null : response.json();
}

const isActive = (pr) => pr.branch && (pr.state === "open" || pr.state === "draft");

// --- What each issue has received (record format: review.mjs) ---

const ledgerPath = (threadId) => `/storage/workspace/${LEDGER_PREFIX}${encodeURIComponent(threadId)}`;

async function readLedger(auth, threadId) {
  try {
    const { value } = await multica(auth, "GET", ledgerPath(threadId));
    const ledger = JSON.parse(value);
    return { ...ledger, sent: ledger.sent ?? {} };
  } catch (error) {
    if (error instanceof UpstreamError && error.status === 404) return { sent: {} };
    throw error;
  }
}

const writeLedger = (auth, threadId, ledger) =>
  multica(auth, "PUT", ledgerPath(threadId), { value: JSON.stringify(ledger) });

// --- Vercel ---

async function vercel(path, query = {}) {
  const url = new URL(path, "https://api.vercel.com");
  url.searchParams.set("teamId", VERCEL_TEAM_ID);
  for (const [key, value] of Object.entries(query)) url.searchParams.set(key, String(value));
  const response = await fetch(url, { headers: { Authorization: `Bearer ${VERCEL_TOKEN}` } });
  if (!response.ok) throw new UpstreamError(response.status, `Vercel ${url.pathname} answered ${response.status}`);
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

const deploymentBranches = new Map();

// The branch a thread's comments are about. A thread on a preview deployed
// with the CLI reports "HEAD"; the deployment it was left on still records
// the branch it was built from. Deployments never change, so each answer is
// kept, including "none" for one that is gone.
async function threadBranch(thread) {
  if (thread.branch && thread.branch !== "HEAD") return thread.branch;
  let host;
  try {
    host = new URL(thread.context?.href ?? "").host;
  } catch {
    return null;
  }
  if (!host) return null;
  if (!deploymentBranches.has(host)) {
    try {
      deploymentBranches.set(host, deploymentBranch(await vercel(`/v13/deployments/${encodeURIComponent(host)}`)));
    } catch (error) {
      if (!(error instanceof UpstreamError) || ![403, 404].includes(error.status)) throw error;
      deploymentBranches.set(host, null);
    }
  }
  return deploymentBranches.get(host);
}

const sameRepo = (repo, pr) =>
  repo && `${repo.owner}/${repo.name}`.toLowerCase() === `${pr.repo_owner}/${pr.repo_name}`.toLowerCase();

const messageIds = (thread) => (thread.messages ?? []).map((message) => message.id);
const lastActivity = (thread) => Math.max(0, ...(thread.messages ?? []).map((message) => message.timestamp ?? 0));

// Sends are serialized so the poller and the issue action cannot both post
// the same messages to one issue.
let sending = Promise.resolve();
function exclusive(task) {
  const run = sending.then(task, task);
  sending = run.catch(() => {});
  return run;
}

// Posts the threads with messages the issue has not received; null when there
// are none. New threads start a conversation on the issue; a follow-up or a
// reopen replies in the conversation its thread started, so a run that has not
// begun yet takes it in.
function sendReview(auth, issueId, candidates) {
  return exclusive(async () => {
    const groups = new Map();
    for (const { pr, thread } of candidates) {
      const ledger = await readLedger(auth, thread.id);
      const plan = classify(thread, ledger, issueId);
      if (!plan) continue;
      const key = plan.parent ?? "";
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push({ pr, thread, ledger, ...plan });
    }
    if (!groups.size) return null;

    const result = { comment_ids: [], threads: [], remaining: 0 };
    for (const [parent, items] of groups) {
      const review = renderReview(items, { teamId: VERCEL_TEAM_ID });
      const { comment, root } = await postReview(auth, issueId, review.content, parent);
      for (const { thread, ledger } of review.sent) {
        try {
          await writeLedger(auth, thread.id, record(ledger, issueId, thread, root));
        } catch (error) {
          console.error(`thread ${thread.id}: sent to issue ${issueId} but not recorded: ${error.message}`);
        }
      }
      result.comment_ids.push(comment.id);
      result.threads.push(...review.sent.map(({ thread }) => thread.id));
      result.remaining += review.remaining;
    }
    return result;
  });
}

async function postReview(auth, issueId, content, parent) {
  const path = `/issues/${encodeURIComponent(issueId)}/comments`;
  if (parent) {
    try {
      const comment = await multica(auth, "POST", path, { content, parent_id: parent, trigger_agents: true });
      return { comment, root: parent };
    } catch (error) {
      // A refused reply posted nothing. When its conversation was deleted the
      // review starts a new one; anything else is a real failure.
      if (!(error instanceof UpstreamError) || ![400, 404].includes(error.status)) throw error;
    }
  }
  const comment = await multica(auth, "POST", path, { content, trigger_agents: true });
  return { comment, root: comment.id };
}

// --- The issue action ---

async function sendPreviewReviewNow(body) {
  if (!body.issue_id) throw new HttpError(422, "this action runs on an issue");
  const auth = hookAuth(body);
  const { pull_requests: linked = [] } = await multica(auth, "GET", `/issues/${encodeURIComponent(body.issue_id)}/pull-requests`);
  const active = linked.filter(isActive);
  const pullRequests = active.length ? active : linked.filter((pr) => pr.branch);
  if (!pullRequests.length) throw new HttpError(422, "the issue has no linked pull request with a branch");

  const threads = await openThreads();
  const items = [];
  const seen = new Set();
  for (const pr of pullRequests) {
    for (const thread of threads) {
      if (seen.has(thread.id) || (await threadBranch(thread)) !== pr.branch) continue;
      if (!sameRepo(await projectRepo(thread.projectId), pr)) continue;
      seen.add(thread.id);
      items.push({ pr, thread });
    }
  }
  const result = items.length ? await sendReview(auth, body.issue_id, items) : null;
  if (!result) throw new HttpError(404, "no new Vercel preview comments on the linked branches");
  return result;
}

// --- The poller ---

// Marks the records of threads that are no longer open, so a reopen is
// recognised, and drops them after CLOSED_RETENTION_MS.
const closedAtCache = new Map();
async function recordClosedThreads(auth, openIds) {
  const { keys = [] } = await multica(auth, "GET", "/storage/workspace");
  for (const { key } of keys) {
    if (!key.startsWith(LEDGER_PREFIX)) continue;
    const threadId = decodeURIComponent(key.slice(LEDGER_PREFIX.length));
    if (openIds.has(threadId)) {
      closedAtCache.delete(threadId);
      continue;
    }
    let closedAt = closedAtCache.get(threadId);
    if (closedAt === undefined) {
      const ledger = await readLedger(auth, threadId);
      closedAt = ledger.closedAt ?? Date.now();
      if (!ledger.closedAt) await writeLedger(auth, threadId, { sent: ledger.sent, closedAt });
      closedAtCache.set(threadId, closedAt);
    }
    if (Date.now() - closedAt > CLOSED_RETENTION_MS) {
      await multica(auth, "DELETE", ledgerPath(threadId));
      closedAtCache.delete(threadId);
    }
  }
}

// Branches whose current comments were already handled, keyed by project and
// branch, so an idle review costs one Vercel call per tick and nothing else.
const handled = new Map();

// True once a branch's comments need no more work: its project deploys no
// known repository, or every issue linked to its open pull request has them.
// False while no open pull request is linked, so a link made later still
// picks the comments up.
async function sendBranch(auth, branch, threads) {
  const { projectId } = threads[0];
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
      console.log(`${repo.owner}/${repo.name}@${branch}: sent ${result.threads.length} threads to issue ${issueId}`);
      if (result.remaining) done = false;
    }
  }
  return done;
}

async function pollOnce() {
  const auth = { base: `${MULTICA_API_URL}/v1`, token: PLUGIN_TOKEN };
  const threads = await openThreads();
  await exclusive(() => recordClosedThreads(auth, new Set(threads.map((thread) => thread.id))));

  const branches = new Map();
  for (const thread of threads) {
    let branch;
    try {
      branch = await threadBranch(thread);
    } catch (error) {
      console.error(`thread ${thread.id}: ${error.message}`);
      continue;
    }
    if (!branch) continue;
    const key = `${thread.projectId} ${branch}`;
    if (!branches.has(key)) branches.set(key, { branch, threads: [] });
    branches.get(key).threads.push(thread);
  }
  for (const key of handled.keys()) if (!branches.has(key)) handled.delete(key);

  for (const [key, { branch, threads: branchThreads }] of branches) {
    const fingerprint = branchThreads.flatMap(messageIds).sort().join(" ");
    if (handled.get(key) === fingerprint) continue;
    // Still reviewing: wait until the newest comment has gone quiet.
    if (Date.now() - Math.max(...branchThreads.map(lastActivity)) < QUIET_MS) continue;
    try {
      if (await sendBranch(auth, branch, branchThreads)) handled.set(key, fingerprint);
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
        console.log(`issue ${body.issue_id}: sent ${result.threads.length} threads on request`);
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
