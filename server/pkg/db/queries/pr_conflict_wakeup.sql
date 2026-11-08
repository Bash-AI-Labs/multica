-- name: ListIssuesNeedingPRConflictWakeup :many
-- Open agent-assigned issues with a linked open GitHub pull request that have
-- never had a "PR has merge conflicts" wakeup. An existing rule (enabled or
-- not) and the creation activity of a deleted one both count, so a rule a
-- member removed is not recreated.
SELECT DISTINCT i.id AS issue_id, i.workspace_id, a.id AS agent_id, a.owner_id
FROM issue i
JOIN agent a ON a.id = i.assignee_id AND a.workspace_id = i.workspace_id
JOIN issue_pull_request ipr ON ipr.issue_id = i.id
JOIN github_pull_request pr ON pr.id = ipr.pull_request_id
WHERE i.assignee_type = 'agent'
  AND a.archived_at IS NULL
  AND a.runtime_id IS NOT NULL
  AND pr.state IN ('open', 'draft')
  AND i.status NOT IN ('done', 'cancelled')
  AND NOT EXISTS (
      SELECT 1 FROM issue_status s
      WHERE s.workspace_id = i.workspace_id
        AND s.key = i.status
        AND s.category IN ('done', 'closed')
  )
  AND NOT EXISTS (
      SELECT 1 FROM issue_wakeup w
      WHERE w.issue_id = i.id
        AND w.condition->>'type' = 'pull_request'
        AND w.condition->>'event' = 'conflicts'
  )
  AND NOT EXISTS (
      SELECT 1 FROM activity_log l
      WHERE l.issue_id = i.id
        AND l.action = 'wakeup_created'
        AND l.details->'wakeup'->'condition'->>'type' = 'pull_request'
        AND l.details->'wakeup'->'condition'->>'event' = 'conflicts'
  )
ORDER BY i.id
LIMIT sqlc.arg(page_limit);
