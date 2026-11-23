# Vercel Preview Review

Anyone on your Vercel team can review an agent's UI change on its Vercel
preview. When they stop commenting, the review goes to the agent that owns the
issue on its own.

1. A teammate opens the pull request's preview deployment and leaves comments
   with the [Vercel Toolbar](https://vercel.com/docs/vercel-toolbar/comments).
   Each comment records the page, the element, the React component tree, the
   viewport and a screenshot.
2. Two minutes after the last comment on that preview, the service posts every
   new open comment to the issues linked to the pull request, as one comment
   that wakes the issue's agent. Each item names the teammate who wrote it.
3. The agent makes the changes in the pull request and posts its updates on
   the Multica issue. It never replies on Vercel. Once an item is done and
   pushed, it resolves the thread, using the command listed under the item.
   That needs Vercel CLI 66 or newer, signed in to the team, on the daemon
   machine.

Reviewers keep the conversation on Vercel, and each Vercel thread keeps one
conversation on the issue:

- New comments start a new review conversation.
- A reply to an open thread goes back as a follow-up in the conversation that
  thread started, showing the original comment and only the new replies.
- Reopening a resolved thread sends it back there marked as reopened, so the
  agent knows the earlier change did not settle it.

A follow-up that arrives before the agent has started joins the run that is
already waiting instead of queueing another one.

To send without waiting, pick **Send preview review to agent** from the issue
actions menu.

## What runs where

| Piece | Where |
| --- | --- |
| `multica.plugin.json` | Uploaded once in **Settings → Plugins** |
| `server/` | A container in your compose stack, reachable only on the compose network as `https://preview-review.internal:8787` |

The container checks the team's open toolbar threads every 30 seconds and
groups them by Vercel project and branch. A group is sent once its newest
message is two minutes old, to the issues linked to an open or draft pull
request from that branch of the project's repository. Branch names like `main`
repeat across projects, so the repository has to match too. A comment on a
preview deployed with the Vercel CLI reports the branch `HEAD`; the service
reads the branch the deployment was built from instead.

The plugin's workspace storage records, per Vercel thread, which messages each
issue received. A message is never sent to the same issue twice, across
restarts and between the automatic path and the issue action. The record of a
resolved thread is kept for 30 days so a reopen is recognised.

The automatic review is written by the plugin itself, and the agent is told it
relays reviewers from Vercel. The run it starts uses the permissions of the
member who installed the plugin and is attributed to them as **Plugin
installer**. The issue action writes as whoever picked it. Reviewer text cannot
mention or wake anyone in Multica: mention links in it are turned into plain
text.

Multica only calls hooks over HTTPS. A one-off `preview-review-cert` container
issues a self-signed certificate for `preview-review.internal` into a shared
volume before the backend starts, and the backend trusts exactly that file
through `MULTICA_PLUGIN_DEV_CA`. The backend waits only for that certificate,
so the service being down never holds the backend back.
`MULTICA_PLUGIN_DEV_ORIGINS` names the one private origin the backend may dial;
the consented `net:` scope still applies. The overlay also turns on plugins
(`FF_PLUGINS_V1`).

## Setup

1. Add to `.env`:

   ```bash
   MULTICA_PLUGIN_SECRET_KEY=   # openssl rand -base64 32, if not already set
   VERCEL_TOKEN=                # https://vercel.com/account/tokens, scoped to the team
   VERCEL_TEAM_ID=team_…        # orgId in any linked project's .vercel/project.json
   ```

2. Start the stack with the overlay listed last:

   ```bash
   docker compose -f docker-compose.selfhost.yml -f docker-compose.selfhost.build.yml \
     -f docker-compose.preview-review.yml up -d
   ```

   With `docker-compose.tailscale.yml`, also set
   `PREVIEW_REVIEW_MULTICA_API_URL=http://tailscale:4900`.

3. Zip `multica.plugin.json` and upload it in **Settings → Plugins**, then
   install it and approve the scopes.

4. Rotate the installation's token and copy both values into `.env`, then
   restart the service:

   ```bash
   PREVIEW_REVIEW_PLUGIN_TOKEN=mpi_…
   PREVIEW_REVIEW_SIGNING_SECRET=whsec_…
   ```

   ```bash
   docker compose … -f docker-compose.preview-review.yml up -d preview-review
   ```

   The log says `sending reviews automatically 120s after the last comment`.
   Change the wait with `PREVIEW_REVIEW_QUIET_SECONDS`.

## When nothing is sent

`docker compose logs preview-review` shows each review it sends and any
failure. The issue action shows the hook's HTTP status:

| Status | Meaning |
| --- | --- |
| 422 | The issue has no linked pull request with a branch |
| 404 | No new open Vercel comments on those branches |
| 401 | `PREVIEW_REVIEW_SIGNING_SECRET` is missing or stale; repeat step 4 |
| 502 | Vercel or Multica refused a call; see the log |

A `403 installer_membership_revoked` in the log means the member who installed
the plugin has left the workspace; reinstall it.

## Tests

```bash
node --test examples/plugins/vercel-preview-review/server/review.test.mjs
```

## Scopes

| Scope | Why |
| --- | --- |
| `issues:read` | Find the issues linked to a preview's pull request |
| `comments:write` | Post the review |
| `tasks:write` | Let that comment wake agents (`trigger_agents`) |
| `storage:workspace` | Record which Vercel messages each issue received |
| `net:preview-review.internal` | The only host the issue action may call |
