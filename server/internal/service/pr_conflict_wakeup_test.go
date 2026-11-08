package service

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// prConflictFixture is an open issue assigned to the fixture agent, linked to
// one GitHub pull request in the given state.
func prConflictFixture(t *testing.T, prState string) (principalFixture, *IssueWakeupService, pgtype.UUID, string) {
	t.Helper()
	f, s, issue, agent := conditionFixture(t)
	f.Exec(t, "UPDATE issue SET assignee_type='agent', assignee_id=$2, status='in_progress' WHERE id=$1", issue, agent)
	pr := f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 1,
		"repo_owner": "fixture", "repo_name": "auto-conflicts", "pr_number": 11,
		"title": "PR", "state": prState, "html_url": "https://example.test/pr/11",
		"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
	})
	f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
	f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	return f, s, issue, agent
}

// ensurePRConflictWakeups runs the sweep over every candidate in the shared
// database, so the fixture issue is never cut off by other tests' rows.
func ensurePRConflictWakeups(t *testing.T, s *IssueWakeupService) {
	t.Helper()
	if _, err := s.EnsurePRConflictWakeups(context.Background(), 100000); err != nil {
		t.Fatal(err)
	}
}

func conflictRuleCount(t *testing.T, f principalFixture, issue pgtype.UUID) int {
	t.Helper()
	return f.Count(t, "SELECT count(*) FROM issue_wakeup WHERE issue_id=$1 AND condition->>'type'='pull_request' AND condition->>'event'='conflicts'", issue)
}

func TestPRConflictWakeupCreatedForAgentIssueWithOpenPR(t *testing.T) {
	f, s, issue, agent := prConflictFixture(t, "open")
	ensurePRConflictWakeups(t, s)

	var id pgtype.UUID
	if err := f.Pool.QueryRow(context.Background(), "SELECT id FROM issue_wakeup WHERE issue_id=$1", issue).Scan(&id); err != nil {
		t.Fatalf("no rule created: %v", err)
	}
	if n := f.Count(t, "SELECT count(*) FROM issue_wakeup WHERE issue_id=$1", issue); n != 1 {
		t.Fatalf("rules = %d, want 1", n)
	}
	w, err := f.q.LocklessWakeup(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if util.UUIDToString(w.AgentID) != agent || util.UUIDToString(w.CreatedBy) != f.UserID {
		t.Fatalf("rule targets agent %s for %s, want agent %s for the owner %s", util.UUIDToString(w.AgentID), util.UUIDToString(w.CreatedBy), agent, f.UserID)
	}
	if w.Kind != "event" || w.Mode != "continuous" || !w.Enabled || w.SystemRule.Valid || w.Instruction != PRConflictWakeupInstruction {
		t.Fatalf("rule = kind %s mode %s enabled %t system %v", w.Kind, w.Mode, w.Enabled, w.SystemRule)
	}
	if conflictRuleCount(t, f, issue) != 1 {
		t.Fatalf("condition = %s", w.Condition)
	}

	ensurePRConflictWakeups(t, s)
	if n := conflictRuleCount(t, f, issue); n != 1 {
		t.Fatalf("second sweep: %d rules, want 1", n)
	}
}

func TestPRConflictWakeupWakesAssigneeOnConflict(t *testing.T) {
	f, s, issue, agent := prConflictFixture(t, "open")
	s.Tasks.PRSnapshotsEnabled = true
	f.Exec(t, "UPDATE github_pull_request SET head_sha='a',snapshot_head_sha='a',api_mergeable='MERGEABLE' WHERE id=(SELECT pull_request_id FROM issue_pull_request WHERE issue_id=$1)", issue)
	ensurePRConflictWakeups(t, s)
	var id pgtype.UUID
	if err := f.Pool.QueryRow(context.Background(), "SELECT id FROM issue_wakeup WHERE issue_id=$1", issue).Scan(&id); err != nil {
		t.Fatalf("no rule created: %v", err)
	}
	wakeTick(t, f, s, id)
	if n := wakeRuns(t, f, id); n != 0 {
		t.Fatalf("mergeable PR started %d runs", n)
	}

	f.Exec(t, "UPDATE github_pull_request SET api_mergeable='CONFLICTING' WHERE id=(SELECT pull_request_id FROM issue_pull_request WHERE issue_id=$1)", issue)
	w := wakeTick(t, f, s, id)
	if n := wakeRuns(t, f, id); n != 1 {
		t.Fatalf("conflicting PR started %d runs, want 1", n)
	}
	task, err := f.q.GetAgentTask(context.Background(), w.LastTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if util.UUIDToString(task.AgentID) != agent || !strings.Contains(task.HandoffNote.String, `"mergeable":"conflicting"`) {
		t.Fatalf("run for agent %s with facts %s", util.UUIDToString(task.AgentID), task.HandoffNote.String)
	}
}

func TestPRConflictWakeupDeletedRuleStaysDeleted(t *testing.T) {
	f, s, issue, _ := prConflictFixture(t, "draft")
	ensurePRConflictWakeups(t, s)
	if conflictRuleCount(t, f, issue) != 1 {
		t.Fatal("draft PR did not get a rule")
	}
	f.Exec(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
	ensurePRConflictWakeups(t, s)
	if n := conflictRuleCount(t, f, issue); n != 0 {
		t.Fatalf("deleted rule recreated: %d rules", n)
	}
}

func TestPRConflictWakeupKeepsAgentsOwnRule(t *testing.T) {
	f, s, issue, agent := prConflictFixture(t, "open")
	wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "My own conflict plan",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "conflicts"})})
	ensurePRConflictWakeups(t, s)
	if n := conflictRuleCount(t, f, issue); n != 1 {
		t.Fatalf("rules = %d, want only the agent's own", n)
	}
}

func TestPRConflictWakeupSkipsIneligibleIssues(t *testing.T) {
	for _, tc := range []struct {
		name, prState, update string
	}{
		{"merged PR", "merged", ""},
		{"closed PR", "closed", ""},
		{"member assignee", "open", "UPDATE issue SET assignee_type='member', assignee_id=$2 WHERE id=$1"},
		{"unassigned", "open", "UPDATE issue SET assignee_type=NULL, assignee_id=NULL WHERE id=$1 AND $2::uuid IS NOT NULL"},
		{"done issue", "open", "UPDATE issue SET status='done' WHERE id=$1 AND $2::uuid IS NOT NULL"},
		{"cancelled issue", "open", "UPDATE issue SET status='cancelled' WHERE id=$1 AND $2::uuid IS NOT NULL"},
		{"archived agent", "open", "UPDATE agent SET archived_at=now() WHERE id=(SELECT assignee_id FROM issue WHERE id=$1) AND $2::uuid IS NOT NULL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s, issue, _ := prConflictFixture(t, tc.prState)
			if tc.update != "" {
				f.Exec(t, tc.update, issue, f.UserID)
			}
			ensurePRConflictWakeups(t, s)
			if n := conflictRuleCount(t, f, issue); n != 0 {
				t.Fatalf("rules = %d, want 0", n)
			}
		})
	}
}

func TestPRConflictWakeupsEnabled(t *testing.T) {
	for value, want := range map[string]bool{"": true, "true": true, "1": true, "false": false, "0": false, "nope": true} {
		t.Setenv("AGENT_PR_CONFLICT_WAKEUPS", value)
		if got := PRConflictWakeupsEnabled(); got != want {
			t.Fatalf("AGENT_PR_CONFLICT_WAKEUPS=%q: %t, want %t", value, got, want)
		}
	}
}
