// What a preview review says and what each issue still needs. Pure functions,
// so the rules that keep reviewer text inert and stop repeat sends are tested
// without Vercel or Multica (review.test.mjs).

// Multica rejects comments over 64 KiB; leave room for the header.
export const MAX_COMMENT_CHARS = 60_000;

const messageIds = (thread) => (thread.messages ?? []).map((message) => message.id);

// --- What each issue has received ---
//
// One record per Vercel thread:
//
//   { sent: { issueId: { messages: [messageId], comment: rootCommentId } },
//     closedAt?, reopened? }
//
// comment is the Multica conversation the thread started on that issue;
// follow-ups and reopens reply there. closedAt is set once the thread leaves
// the open list; seeing it open again means a reviewer reopened it.

// What issueId still needs from thread, or null when it has every message.
// parent is the conversation to reply in, if the thread already has one.
export function classify(thread, ledger, issueId) {
  const entry = ledger.sent?.[issueId];
  const received = new Set(ledger.closedAt ? [] : (entry?.messages ?? []));
  const fresh = new Set(messageIds(thread).filter((id) => !received.has(id)));
  if (!fresh.size) return null;
  const reopened = Boolean(ledger.closedAt) || (Boolean(ledger.reopened) && Boolean(entry) && received.size === 0);
  return { fresh, reopened, followUp: !reopened && received.size > 0, parent: entry?.comment };
}

// The record after issueId received every current message of thread in the
// conversation rooted at commentId. A reopened thread starts a new round, so
// every issue that had it gets it again.
export function record(ledger, issueId, thread, commentId) {
  const sent = {};
  for (const [id, entry] of Object.entries(ledger.sent ?? {})) {
    sent[id] = ledger.closedAt ? { ...entry, messages: [] } : entry;
  }
  sent[issueId] = { messages: messageIds(thread), comment: commentId };
  return ledger.closedAt || ledger.reopened ? { sent, reopened: true } : { sent };
}

// --- Which branch a comment is on ---

// The branch a Vercel deployment was built from. Git-integration deployments
// name it in <provider>CommitRef. The CLI records the checkout's ref in
// gitCommitRef, which is "HEAD" on a detached checkout, and the branch it was
// built from in githubRef.
export function deploymentBranch(deployment) {
  const meta = deployment?.meta ?? {};
  const refs = [
    meta.githubCommitRef,
    meta.gitlabCommitRef,
    meta.bitbucketCommitRef,
    meta.githubRef,
    deployment?.gitSource?.ref,
    meta.gitCommitRef,
  ];
  return refs.find((ref) => typeof ref === "string" && ref !== "" && ref !== "HEAD") ?? null;
}

// --- Rendering ---

// Everything from Vercel is reviewer input. It must not mention or wake anyone
// in Multica, hide text in an HTML comment, or break out of the markdown
// around it.
export function inert(text) {
  return String(text ?? "")
    .replace(/\[([^\]]*)\]\(\s*mention:[^)]*\)/gi, "$1")
    .replace(/mention:\/\//gi, "mention:\u200b//")
    .replace(/<!--/g, "&lt;!--");
}
const inline = (text) => inert(text).replace(/\s+/g, " ").replace(/[`[\]]/g, "").trim();
const quote = (text) => inert(text).trim().split("\n").map((line) => `> ${line}`).join("\n");
const token = (value) => String(value ?? "").replace(/[^A-Za-z0-9_-]/g, "");
const httpsURL = (value) => (/^https:\/\/[^\s()<>]+$/.test(String(value ?? "")) ? value : null);
const reviewer = (message) => inline(message?.author?.name || message?.author?.username) || "A reviewer";

function joinNames(names) {
  if (names.length <= 1) return names.join("");
  return `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
}

// The React tree Vercel captures runs root -> selected element; the last few
// lines are the components that actually render the commented element.
function componentPath(tree) {
  const lines = String(tree).split("\n").filter((line) => line.trim());
  const selected = lines.find((line) => line.startsWith("Selected:"));
  const tail = lines.slice(-6);
  const indent = Math.min(...tail.map((line) => line.match(/^ */)[0].length));
  return [selected, ...tail.map((line) => line.slice(indent))]
    .filter(Boolean)
    .map((line) => line.replace(/`/g, "'").slice(0, 200));
}

function images(message) {
  return (message?.attachments ?? [])
    .filter((attachment) => attachment.mimeType?.startsWith("image/") && httpsURL(attachment.url))
    .map((attachment) => `![${inline(attachment.filename) || "screenshot"}](${attachment.url})`);
}

function renderMessage(message) {
  const text = message.text?.trim() ? quote(message.text) : "> (screenshot only)";
  // A blank line keeps screenshots out of the quote.
  return [`**${reviewer(message)}:**\n${text}`, ...images(message)].join("\n\n");
}

// One thread as the agent needs it. A follow-up shows the original comment for
// context and then only the replies this issue has not seen.
function renderThread({ thread, fresh, reopened, followUp }, index, teamId) {
  const [first, ...replies] = thread.messages ?? [];
  const context = thread.context ?? {};
  const device = context.device ?? {};
  const where = inline(context.path ?? context.href) || "unknown page";
  const page = httpsURL(context.href) ? `[${where}](${context.href})` : where;
  const viewport = device.screenWidth
    ? ` · ${token(device.screenWidth)}×${token(device.screenHeight)} @${Number(device.devicePixelRatio) || 1}x`
    : "";
  const label = reopened ? "Reopened: " : followUp ? "Follow-up: " : "";
  const newReplies = replies.filter((reply) => fresh.has(reply.id));
  const author = reviewer(followUp ? newReplies[0] : first);

  const lines = [`### ${index + 1}. ${label}${author} on ${where}`, ""];
  if (reopened) lines.push("A reviewer reopened this thread after it was resolved, so the earlier change did not settle it.", "");
  if (followUp) {
    lines.push(`Original comment by ${reviewer(first)}:`, quote(first?.text || "(screenshot only)"), "", "New replies:", "");
    for (const reply of newReplies) lines.push(renderMessage(reply), "");
  } else {
    for (const message of [first, ...replies]) lines.push(renderMessage(message), "");
  }
  lines.push(`- **Page:** ${page}${viewport}`);
  if (context.selector) lines.push(`- **Element:** \`${String(context.selector).replace(/`/g, "'").slice(0, 300)}\``);
  const threadURL = httpsURL(thread.webUrl);
  lines.push(`- **Vercel thread:** ${threadURL ? `[${token(thread.id)}](${threadURL})` : token(thread.id)}`);
  if (context.frameworkContext) lines.push("", "```text", ...componentPath(context.frameworkContext), "```");
  lines.push(
    "",
    "Resolve when done:",
    "",
    "```sh",
    `vercel comments resolve ${token(thread.id)} --project ${token(thread.projectId)} --scope ${token(teamId)} --yes`,
    "```",
  );
  return lines.join("\n").trimEnd();
}

// One comment for items ({ pr, thread, fresh, reopened, followUp }), oldest
// thread first. Returns the items it carries; the rest go out next time.
export function renderReview(items, { teamId }) {
  const ordered = [...items].sort(
    (a, b) => (a.thread.messages?.[0]?.timestamp ?? 0) - (b.thread.messages?.[0]?.timestamp ?? 0),
  );
  const prs = [...new Map(ordered.map(({ pr }) => [pr.id, pr])).values()];
  const reviewers = [
    ...new Set(
      ordered.flatMap(({ thread, fresh }) => (thread.messages ?? []).filter((m) => fresh.has(m.id)).map(reviewer)),
    ),
  ];
  // The first line stays plain text: run lists and notifications show it as
  // the trigger summary.
  const header = [
    `Preview review from ${joinNames(reviewers)}: ${ordered.length} comment${ordered.length === 1 ? "" : "s"}.`,
    "",
    "On " +
      prs
        .map((pr) => {
          const link = `${inline(pr.repo_name)}#${token(pr.number)}`;
          return `${httpsURL(pr.html_url) ? `[${link}](${pr.html_url})` : link} (\`${inline(pr.branch)}\`)`;
        })
        .join(", ") +
      ". Make each change in its pull request and post your updates here, on this issue. " +
      "Do not reply on Vercel. Once an item is done and pushed, resolve its Vercel thread with the command under it.",
  ].join("\n");

  let content = header;
  const sent = [];
  for (const [index, item] of ordered.entries()) {
    const section = `\n\n---\n\n${renderThread(item, index, teamId)}`;
    if (sent.length && content.length + section.length > MAX_COMMENT_CHARS) {
      content += `\n\n---\n\n…and ${ordered.length - index} more open comments on Vercel, sent next.`;
      break;
    }
    content += section;
    sent.push(item);
  }
  return { content, sent, remaining: ordered.length - sent.length };
}
