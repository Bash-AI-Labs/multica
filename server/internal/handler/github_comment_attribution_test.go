package handler

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestToolInputWritesPRComment(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"pr comment with number", `{"command": "gh pr comment 1552 --body-file /tmp/r.md"}`, true},
		{"pr comment after cd and line continuation", `{"command": "cd /w/bashpos && \\\ngh pr comment 1552 --body-file /tmp/r.md"}`, true},
		{"pr comment on current branch", `{"command": "gh pr comment --body-file /tmp/r.md"}`, true},
		{"pr comment by url", `{"command": "gh pr comment https://github.com/o/r/pull/1552 -b done"}`, true},
		{"pr review with body", `{"command": "gh pr review 1552 --comment -b looks good"}`, true},
		{"pr comment on another pr", `{"command": "gh pr comment 1551 --body done"}`, false},
		{"api reply to review thread", `{"command": "gh api repos/o/r/pulls/1552/comments/4580772413/replies \\\n -f body=\"Fixed in abc\""}`, true},
		{"api inline review comment", `{"command": "gh api repos/o/r/pulls/1552/comments --method POST -F body=@./r.md -f commit_id=abc"}`, true},
		{"api issue comment", `{"command": "gh api repos/o/r/issues/1552/comments -f body=done"}`, true},
		{"api read with jq", `{"command": "gh api repos/o/r/pulls/1552/comments --paginate --jq '.[] | {id, body}'"}`, false},
		{"api read with fields", `{"command": "gh api repos/o/r/issues/1552/comments -X GET -f per_page=100"}`, false},
		{"api write to another pr", `{"command": "gh api repos/o/r/issues/1551/comments -f body=done"}`, false},
		{"view only", `{"command": "gh pr view 1552 --json body && gh api repos/o/r/pulls/1552/comments"}`, false},
		{"graphql thread reply", `{"command": "gh api graphql -f query='mutation { addPullRequestReviewThreadReply(input: {body: \"ok\"}) { comment { id } } }'"}`, true},
		{"argv command", `{"command": ["gh", "pr", "comment", "1552", "--body", "done"]}`, true},
		{"file content is not a command", `{"path": "notes.md", "file_text": "gh pr comment 1552 --body x"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolInputWritesPRComment([]byte(tc.input), 1552); got != tc.want {
				t.Fatalf("toolInputWritesPRComment(%s) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// Agents post PR comments under the workspace owner's GitHub login, so the
// webhook for an agent's own reply must not wake that agent again.
func TestWebhook_PRComment_SkipsAgentsOwnComment(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	secret := "pr-comment-self-echo-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	const (
		installationID int64 = 77441122
		prNumber             = 4242
		ownerLogin           = "agents-owner"
	)
	dbfx.Insert(t, "github_installation", testutil.Cols{
		"workspace_id":    testWorkspaceID,
		"installation_id": installationID,
		"account_login":   "acme",
	})
	prID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id":    testWorkspaceID,
		"installation_id": installationID,
		"repo_owner":      "acme",
		"repo_name":       "widget",
		"pr_number":       prNumber,
		"title":           "Fix login",
		"state":           "open",
		"html_url":        "https://github.com/acme/widget/pull/4242",
		"author_login":    ownerLogin,
		"pr_created_at":   testutil.Raw("now()"),
		"pr_updated_at":   testutil.Raw("now()"),
	})
	runtimeID := dbfx.Runtime(t, "self-echo runtime")
	author := dbfx.Agent(t, "self-echo author", runtimeID)
	reviewer := dbfx.Agent(t, "self-echo reviewer", runtimeID)
	authorIssue := dbfx.Issue(t, "self-echo author issue", testutil.Cols{"status": "in_review", "assignee_type": "agent", "assignee_id": author})
	reviewerIssue := dbfx.Issue(t, "self-echo reviewer issue", testutil.Cols{"status": "in_review", "assignee_type": "agent", "assignee_id": reviewer})
	for _, issueID := range []string{authorIssue, reviewerIssue} {
		dbfx.InsertNoID(t, "issue_pull_request", testutil.Cols{"issue_id": issueID, "pull_request_id": prID}, "issue_id = $1", issueID)
		dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, issueID)
		dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1`, issueID)
	}
	running := func(agentID, issueID string) string {
		return dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "running", "started_at": testutil.Raw("now() - interval '1 minute'")})
	}
	postedComment := func(taskID string, seq int) {
		dbfx.Insert(t, "task_message", testutil.Cols{
			"task_id": taskID,
			"seq":     seq,
			"type":    "tool_use",
			"tool":    "bash",
			"input":   testutil.Raw(`'{"command": "gh pr comment 4242 --body-file /tmp/reply.md"}'::jsonb`),
		})
	}
	sendComment := func(login, body string) {
		raw, _ := json.Marshal(map[string]any{
			"action":       "created",
			"comment":      map[string]any{"body": body, "user": map[string]any{"login": login, "type": "User"}},
			"issue":        map[string]any{"number": prNumber, "pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widget/pulls/4242"}},
			"repository":   map[string]any{"name": "widget", "full_name": "acme/widget", "owner": map[string]any{"login": "acme"}},
			"installation": map[string]any{"id": installationID},
		})
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(raw)
		req := httptest.NewRequest("POST", "/api/webhooks/github", bytes.NewReader(raw))
		req.Header.Set("X-GitHub-Event", "issue_comment")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		testutil.Call(t, testHandler.HandleGitHubWebhook, req).Want(http.StatusAccepted)
	}
	mirrored := func(issueID, body string) int {
		return dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%' || $2`, issueID, body)
	}
	queued := func(issueID string) int {
		return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, issueID)
	}

	postedComment(running(author, authorIssue), 1)

	sendComment(ownerLogin, "Fixed in abc123.")
	if n := mirrored(authorIssue, "Fixed in abc123."); n != 0 {
		t.Fatalf("author's own reply mirrored into its issue %d times, want 0", n)
	}
	if n := queued(authorIssue); n != 0 {
		t.Fatalf("author's own reply queued %d runs for the author, want 0", n)
	}
	if n := mirrored(reviewerIssue, "Fixed in abc123."); n != 1 {
		t.Fatalf("author's reply reached the reviewer %d times, want 1", n)
	}

	sendComment("teammate", "Please also cover the empty case.")
	if n := mirrored(authorIssue, "Please also cover the empty case."); n != 1 {
		t.Fatalf("a teammate's comment reached the author %d times, want 1", n)
	}

	postedComment(running(reviewer, reviewerIssue), 1)
	sendComment(ownerLogin, "Two agents replied at once.")
	if n := mirrored(authorIssue, "Two agents replied at once."); n != 1 {
		t.Fatalf("an ambiguous comment reached the author %d times, want 1", n)
	}
}
