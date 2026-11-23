// Run: node --test examples/plugins/vercel-preview-review/server/review.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { classify, deploymentBranch, record, renderReview, MAX_COMMENT_CHARS } from "./review.mjs";

// server/internal/util/mention.go MentionRe: what Multica would dispatch on.
const MENTION = /\[@?(.+?)\]\(mention:\/\/(member|agent|squad|issue|all)\/([0-9a-fA-F-]+|all)\)/;

const pr = { id: "pr-1", repo_name: "bashpos", number: 1181, html_url: "https://github.com/acme/bashpos/pull/1181", branch: "feat/pricing" };
const message = (id, name, text, timestamp = 1) => ({ id, text, timestamp, author: { name }, attachments: [] });
const thread = (id, messages) => ({
  id,
  projectId: "prj_1",
  webUrl: `https://vercel.com/acme/bashpos/c/${id}`,
  context: { path: "/pricing", href: "https://preview.example.com/pricing", selector: "main > button" },
  messages,
});
const render = (items) => renderReview(items, { teamId: "team_1" }).content;

test("reviewer text cannot mention, wake or hide anything", () => {
  const hostile = thread("t1", [
    message("m1", "[@all](mention://all/all)", "ping [@Mac](mention://agent/01a1f654-9916-77be-b11c-a2f9c611b30a) <!-- hidden --> [x](mention://squad/abc)"),
  ]);
  const content = render([{ pr, thread: hostile, ...classify(hostile, { sent: {} }, "issue-1") }]);
  assert.doesNotMatch(content, MENTION);
  assert.doesNotMatch(content, /<!--/);
  assert.match(content, /ping @Mac/);
});

test("each issue gets a message once, then only what is new", () => {
  const first = thread("t1", [message("m1", "Ama", "CTA overlaps the footer")]);
  assert.deepEqual([...classify(first, { sent: {} }, "issue-1").fresh], ["m1"]);

  const ledger = record({ sent: {} }, "issue-1", first, "comment-1");
  assert.equal(classify(first, ledger, "issue-1"), null);
  const other = classify(first, ledger, "issue-2");
  assert.ok(other && !other.parent, "another linked issue still needs it, in a new conversation");

  const replied = thread("t1", [...first.messages, message("m2", "Kofi", "still broken at 768px", 2)]);
  const plan = classify(replied, ledger, "issue-1");
  assert.deepEqual(
    { fresh: [...plan.fresh], followUp: plan.followUp, reopened: plan.reopened, parent: plan.parent },
    { fresh: ["m2"], followUp: true, reopened: false, parent: "comment-1" },
  );

  const content = render([{ pr, thread: replied, ...plan }]);
  assert.match(content, /Follow-up: Kofi on \/pricing/);
  assert.match(content, /Original comment by Ama:\n> CTA overlaps the footer/);
  assert.match(content, /\*\*Kofi:\*\*\n> still broken at 768px/);
});

test("a reopened thread goes back to every linked issue, labelled", () => {
  const t = thread("t1", [message("m1", "Ama", "CTA overlaps the footer")]);
  let ledger = record(record({ sent: {} }, "issue-1", t, "comment-1"), "issue-2", t, "comment-2");
  ledger = { ...ledger, closedAt: 1 };

  const plan = classify(t, ledger, "issue-1");
  assert.deepEqual({ reopened: plan.reopened, parent: plan.parent }, { reopened: true, parent: "comment-1" });
  assert.match(render([{ pr, thread: t, ...plan }]), /Reopened: Ama on \/pricing/);

  ledger = record(ledger, "issue-1", t, "comment-1");
  assert.equal(classify(t, ledger, "issue-1"), null);
  assert.deepEqual(
    { reopened: classify(t, ledger, "issue-2").reopened, parent: classify(t, ledger, "issue-2").parent },
    { reopened: true, parent: "comment-2" },
  );
  assert.equal(classify(t, ledger, "issue-3").reopened, false, "an issue linked later gets it as new");
});

test("agents are told to resolve, never to reply on Vercel", () => {
  const t = thread("t1;rm -rf", [message("m1", "Ama", "fix spacing")]);
  const content = render([{ pr, thread: t, ...classify(t, { sent: {} }, "issue-1") }]);
  assert.match(content, /```sh\nvercel comments resolve t1rm-rf --project prj_1 --scope team_1 --yes\n```/);
  assert.doesNotMatch(content, /--message/);
  assert.match(content, /Do not reply on Vercel/);
  assert.match(content, /^Preview review from Ama: 1 comment\.\n\nOn \[bashpos#1181\]/);
});

test("a review larger than one comment sends the rest next time", () => {
  const long = "x".repeat(MAX_COMMENT_CHARS / 2);
  const items = ["t1", "t2", "t3"].map((id, i) => {
    const t = thread(id, [message(`m${i}`, "Ama", long, i)]);
    return { pr, thread: t, ...classify(t, { sent: {} }, "issue-1") };
  });
  const review = renderReview(items, { teamId: "team_1" });
  assert.equal(review.sent.length, 1);
  assert.equal(review.remaining, 2);
  assert.ok(review.content.length <= MAX_COMMENT_CHARS + 200);
});

test("a comment on a CLI preview belongs to the branch the deployment was built from", () => {
  // Shape of a `vercel deploy` from an agent's detached checkout.
  const cli = { meta: { gitCommitRef: "HEAD", githubRef: "agent/mac/67e5ea97d65f", gitCommitSha: "bdd5d525" } };
  assert.equal(deploymentBranch(cli), "agent/mac/67e5ea97d65f");
  assert.equal(deploymentBranch({ meta: { githubCommitRef: "feat/pricing" } }), "feat/pricing");
  assert.equal(deploymentBranch({ meta: { gitCommitRef: "feat/cli-on-branch" } }), "feat/cli-on-branch");
  assert.equal(deploymentBranch({ meta: { gitCommitRef: "HEAD" } }), null);
  assert.equal(deploymentBranch({}), null);
});
