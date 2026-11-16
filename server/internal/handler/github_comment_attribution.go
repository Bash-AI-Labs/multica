package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// prCommentAttributionWindow bounds how far back a PR comment is matched to
// the command that posted it. One tool call can run checks for minutes before
// it posts, so this is wider than webhook latency.
const prCommentAttributionWindow = 5 * time.Minute

var (
	ghPRCommentCmd  = regexp.MustCompile(`\bgh\s+pr\s+(?:comment|review)\b([^;&|\n]*)`)
	ghAPICommentCmd = regexp.MustCompile(`\bgh\s+api\b[^;&|\n]*?/(?:pulls|issues)/(\d+)/(?:comments|reviews)\b[^;&|\n]*`)
	ghGraphQLWrite  = regexp.MustCompile(`\bgh\s+api\s+graphql\b[\s\S]*\b(?:addComment|addPullRequestReview|addPullRequestReviewComment|addPullRequestReviewThread|addPullRequestReviewThreadReply|submitPullRequestReview)\b`)
	ghAPIWriteFlag  = regexp.MustCompile(`(?:^|\s)(?:-f|-F|--field|--raw-field|--input|-X\s*POST|--method[\s=]+POST)(?:\s|=|$)`)
	ghAPIReadMethod = regexp.MustCompile(`(?:^|\s)(?:-X\s*GET|--method[\s=]+GET)(?:\s|$)`)
	ghPRRefArg      = regexp.MustCompile(`^\s*(?:https://\S*/pull/)?#?(\d+)\b`)
)

// prCommentSelfAuthor returns the agent that posted a PR comment, when the
// comment came from the PR author's login and exactly one agent working on
// the PR's issues ran a command writing a comment to it within the window.
// Agents post under the workspace owner's GitHub account, so the login alone
// cannot tell an agent's own reply from someone else's comment.
func (h *Handler) prCommentSelfAuthor(ctx context.Context, pr db.GithubPullRequest, issueIDs []pgtype.UUID, commentAuthor string) pgtype.UUID {
	if !pr.AuthorLogin.Valid || !strings.EqualFold(pr.AuthorLogin.String, commentAuthor) {
		return pgtype.UUID{}
	}
	rows, err := h.Queries.ListRecentToolInputsForIssues(ctx, db.ListRecentToolInputsForIssuesParams{
		IssueIds: issueIDs,
		Since:    pgtype.Timestamptz{Time: time.Now().Add(-prCommentAttributionWindow), Valid: true},
	})
	if err != nil {
		slog.Warn("github: list recent tool calls for pr comment attribution", "err", err, "pr", pr.PrNumber)
		return pgtype.UUID{}
	}
	var writer pgtype.UUID
	for _, row := range rows {
		if !toolInputWritesPRComment(row.Input, pr.PrNumber) {
			continue
		}
		if writer.Valid && writer != row.AgentID {
			return pgtype.UUID{}
		}
		writer = row.AgentID
	}
	return writer
}

// toolInputWritesPRComment reports whether a shell tool call posts a comment
// or review to PR prNumber. `gh pr comment|review` without a PR argument
// targets the checkout's branch, which the caller already scopes to this PR.
func toolInputWritesPRComment(input []byte, prNumber int32) bool {
	command := toolCommand(input)
	if command == "" {
		return false
	}
	command = strings.ReplaceAll(command, "\\\n", " ")
	want := strconv.Itoa(int(prNumber))
	for _, m := range ghPRCommentCmd.FindAllStringSubmatch(command, -1) {
		if ref := ghPRRefArg.FindStringSubmatch(m[1]); ref == nil || ref[1] == want {
			return true
		}
	}
	for _, m := range ghAPICommentCmd.FindAllStringSubmatch(command, -1) {
		if m[1] == want && ghAPIWriteFlag.MatchString(m[0]) && !ghAPIReadMethod.MatchString(m[0]) {
			return true
		}
	}
	return ghGraphQLWrite.MatchString(command)
}

// toolCommand extracts the shell command from a tool call's input. Agent
// runtimes record it as a string or an argv array under "command".
func toolCommand(input []byte) string {
	var call struct {
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal(input, &call) != nil || len(call.Command) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(call.Command, &s) == nil {
		return s
	}
	var argv []string
	if json.Unmarshal(call.Command, &argv) == nil {
		return strings.Join(argv, " ")
	}
	return ""
}
