package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Agents resolve merge conflicts on the pull requests linked to their issues.
// EnsurePRConflictWakeups gives every open agent-assigned issue with a linked
// open GitHub pull request one continuous "PR has merge conflicts" wakeup that
// targets the assignee, the same rule an agent can register itself with
// `multica issue wakeup create <issue> --until-pr conflicts --mode continuous`.
// It is an ordinary rule created on behalf of the agent's owner, so members
// see, pause or delete it in the issue sidebar; an issue that ever had such a
// rule is skipped, so a deleted rule stays deleted. Detection needs the GitHub
// App snapshot client (GITHUB_APP_ID / GITHUB_APP_PRIVATE_KEY); without it the
// rule records the condition's diagnostic and waits.

// PRConflictWakeupInstruction is what the woken agent is asked to do.
const PRConflictWakeupInstruction = "A pull request linked to this issue has merge conflicts with its base branch; the trigger facts list the PR number and head. Resolve them on that same pull request:\n" +
	"1. Check out the PR's head branch and fetch the latest base branch.\n" +
	"2. Merge the base branch into the PR branch. Do not rebase or force-push, so reviewers keep their history.\n" +
	"3. Resolve every conflict by keeping the intent of both sides. Never drop changes that landed on the base branch just to make yours apply.\n" +
	"4. Regenerate generated files (lockfiles, codegen) instead of hand-merging them, then run the relevant build, lint and tests and fix anything the merge broke.\n" +
	"5. Push to the same branch; do not open a new pull request.\n" +
	"6. Comment on this issue with what conflicted and how you resolved it.\n" +
	"If a conflict needs a product or design decision you cannot make safely, or the pull request is no longer needed, leave the branch unchanged, comment with the options, and stop."

var prConflictCondition = json.RawMessage(`{"type":"pull_request","event":"conflicts"}`)

// PRConflictWakeupsEnabled reads AGENT_PR_CONFLICT_WAKEUPS; only an explicit
// false turns the sweep off.
func PRConflictWakeupsEnabled() bool {
	raw := strings.TrimSpace(os.Getenv("AGENT_PR_CONFLICT_WAKEUPS"))
	if raw == "" {
		return true
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		slog.Warn("invalid AGENT_PR_CONFLICT_WAKEUPS; using default", "value", raw, "default", true)
		return true
	}
	return enabled
}

// EnsurePRConflictWakeups creates missing rules for up to limit issues and
// returns how many it created.
func (s *IssueWakeupService) EnsurePRConflictWakeups(ctx context.Context, limit int32) (int, error) {
	candidates, err := s.Tasks.Queries.ListIssuesNeedingPRConflictWakeup(ctx, limit)
	if err != nil {
		return 0, err
	}
	created := 0
	var errs []error
	for _, c := range candidates {
		member, err := s.prConflictRuleMember(ctx, c)
		if err != nil {
			errs = append(errs, fmt.Errorf("issue %s: %w", util.UUIDToString(c.IssueID), err))
			continue
		}
		if !member.Valid {
			slog.Debug("pr conflict wakeup: no member may invoke the assignee", "issue_id", util.UUIDToString(c.IssueID), "agent_id", util.UUIDToString(c.AgentID))
			continue
		}
		_, err = s.Create(ctx, c.IssueID, member, pgtype.UUID{}, WakeupInput{
			AgentID:     util.UUIDToString(c.AgentID),
			Instruction: PRConflictWakeupInstruction,
			Kind:        "event",
			Mode:        "continuous",
			Condition:   prConflictCondition,
		})
		switch {
		case err == nil:
			created++
			slog.Info("pr conflict wakeup created", "issue_id", util.UUIDToString(c.IssueID), "agent_id", util.UUIDToString(c.AgentID))
		case errors.Is(err, ErrWakeupInput), errors.Is(err, ErrWakeupForbidden), errors.Is(err, pgx.ErrNoRows):
			// The issue closed or the agent changed since the candidate query.
			slog.Debug("pr conflict wakeup skipped", "issue_id", util.UUIDToString(c.IssueID), "error", err)
		default:
			errs = append(errs, fmt.Errorf("issue %s: %w", util.UUIDToString(c.IssueID), err))
		}
	}
	return created, errors.Join(errs...)
}

// prConflictRuleMember is who the rule is created on behalf of: the agent's
// owner, else a workspace owner or admin allowed to invoke the agent.
func (s *IssueWakeupService) prConflictRuleMember(ctx context.Context, c db.ListIssuesNeedingPRConflictWakeupRow) (pgtype.UUID, error) {
	q := s.Tasks.Queries
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: c.AgentID, WorkspaceID: c.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	if c.OwnerID.Valid {
		if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: c.OwnerID, WorkspaceID: c.WorkspaceID}); err == nil {
			return c.OwnerID, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, err
		}
	}
	members, err := q.ListMembers(ctx, c.WorkspaceID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	for _, m := range members {
		if (m.Role == "owner" || m.Role == "admin") && CanMemberInvokeAgent(ctx, q, agent, m.UserID, c.WorkspaceID) {
			return m.UserID, nil
		}
	}
	return pgtype.UUID{}, nil
}
