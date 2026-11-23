# Vercel Preview Review

Review an agent's UI change on its Vercel preview. When you stop commenting,
the review goes to the agent on its own.

1. Open the pull request's preview deployment and leave comments with the
   [Vercel Toolbar](https://vercel.com/docs/vercel-toolbar/comments). Each
   comment records the page, the element you clicked, the React component tree,
   the viewport and a screenshot.
2. Two minutes after your last comment on that preview, the service posts every
   new open comment to the issues linked to the pull request, as one comment
   that wakes the issue's agent.
3. The agent fixes each item and closes its thread with
   `vercel comments resolve <thread> --message "<what changed>"`. That needs
   Vercel CLI 66 or newer, signed in, on the daemon machine.

Replies and new comments after that start a new batch. To send without waiting,
pick **Send preview review to agent** from the issue actions menu.

## What runs where

| Piece | Where |
| --- | --- |
| `multica.plugin.json` | Uploaded once in **Settings → Plugins** |
| `server/` | A container in your compose stack, reachable only on the compose network as `https://preview-review.internal:8787` |

The container checks the team's open toolbar threads every 30 seconds and
groups them by Vercel project and branch. A group is sent once its newest
message is two minutes old, to the issues linked to an open or draft pull
request from that branch of the project's repository. Branch names like `main`
repeat across projects, so the repository has to match too. Previews deployed
from the CLI report the branch `HEAD` and are not matched.

Each review comment ends with a hidden list of the Vercel messages it carried,
so a message is never sent to the same issue twice, across restarts and
between the automatic path and the issue action.

The comment is written as the member who installed the plugin and marked with
the plugin, and the agent run it starts is attributed to that member. The
issue action writes as whoever picked it.

Multica only calls hooks over HTTPS. On first start the container issues a
self-signed certificate for `preview-review.internal` into a shared volume, and
the backend trusts exactly that file through `MULTICA_PLUGIN_DEV_CA`.
`MULTICA_PLUGIN_DEV_ORIGINS` names the one private origin the backend may dial;
the consented `net:` scope still applies.

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

## Scopes

| Scope | Why |
| --- | --- |
| `issues:read` | Find the issues linked to a preview's pull request |
| `comments:read` | See which Vercel messages an issue already received |
| `comments:write` | Post the review |
| `tasks:write` | Let that comment wake agents (`trigger_agents`) |
| `net:preview-review.internal` | The only host the issue action may call |
