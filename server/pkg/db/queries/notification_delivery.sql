-- name: ListPendingNotificationDeliveries :many
SELECT
    i.id,
    i.workspace_id,
    i.recipient_id,
    i.type,
    i.created_at,
    u.email AS recipient_email,
    u.name AS recipient_name,
    w.slug AS workspace_slug,
    w.name AS workspace_name,
    w.issue_prefix,
    issue.number AS issue_number,
    issue.title AS issue_title,
    COALESCE(actor_agent.name, actor_user.name, 'Someone') AS actor_name,
    COALESCE(preference.preferences, '{}'::jsonb) AS preferences
FROM inbox_item i
JOIN "user" u
    ON u.id = i.recipient_id
JOIN member m
    ON m.workspace_id = i.workspace_id
   AND m.user_id = i.recipient_id
JOIN workspace w
    ON w.id = i.workspace_id
LEFT JOIN issue
    ON issue.id = i.issue_id
   AND issue.workspace_id = i.workspace_id
LEFT JOIN agent actor_agent
    ON i.actor_type = 'agent'
   AND actor_agent.id = i.actor_id
   AND actor_agent.workspace_id = i.workspace_id
LEFT JOIN "user" actor_user
    ON i.actor_type = 'member'
   AND actor_user.id = i.actor_id
LEFT JOIN notification_preference preference
    ON preference.workspace_id = i.workspace_id
   AND preference.user_id = i.recipient_id
WHERE i.recipient_type = 'member'
  AND i.read = false
  AND i.archived = false
  AND i.type = ANY(sqlc.arg('types')::text[])
  AND i.created_at > sqlc.arg('after')
  AND i.created_at <= sqlc.arg('before')
ORDER BY i.workspace_id, i.recipient_id, i.created_at, i.id;
