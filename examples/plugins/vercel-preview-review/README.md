# Vercel Preview Review

Review an agent's UI change on its Vercel preview, then hand the whole review
to the agent in one click.

1. Open the pull request's preview deployment and leave comments with the
   [Vercel Toolbar](https://vercel.com/docs/vercel-toolbar/comments). Each
   comment records the page, the element you clicked, the React component tree,
   the viewport and a screenshot.
2. In Multica, open the issue and pick **Send preview review to agent** from the
   issue actions menu.
3. The service collects every open toolbar thread on the branches of the
   issue's linked pull requests and posts them as one comment. The comment is
   authored by you and wakes the issue's agent, exactly as if you had written
   it.
4. The agent fixes each item and closes its thread with
   `vercel comments resolve <thread> --message "<what changed>"`. That needs
   Vercel CLI 66 or newer, signed in, on the daemon machine.

## What runs where

| Piece | Where |
| --- | --- |
| `multica.plugin.json` | Uploaded once in **Settings → Plugins** |
| `server/` | A container in your compose stack, reachable only on the compose network as `https://preview-review.internal:8787` |

Multica only calls hooks over HTTPS. On first start the container issues a
self-signed certificate for `preview-review.internal` into a shared volume, and
the backend trusts exactly that file through `MULTICA_PLUGIN_DEV_CA`.
`MULTICA_PLUGIN_DEV_ORIGINS` names the one private origin the backend may dial;
the consented `net:` scope still applies.

Threads are matched by branch, and kept only when their Vercel project deploys
the pull request's repository, because branch names like `main` repeat across
projects. Previews deployed from the CLI report the branch `HEAD` and are not
matched.

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

4. Rotate the installation's token. Copy the signing secret (`whsec_…`) into
   `.env` as `PREVIEW_REVIEW_SIGNING_SECRET`, then restart the service:

   ```bash
   docker compose … -f docker-compose.preview-review.yml up -d preview-review
   ```

## When nothing is sent

The issue menu shows the hook's HTTP status:

| Status | Meaning |
| --- | --- |
| 422 | The issue has no linked pull request with a branch |
| 404 | No open Vercel comments on those branches |
| 401 | `PREVIEW_REVIEW_SIGNING_SECRET` is missing or stale; repeat step 4 |
| 502 | Vercel or Multica refused a call; see `docker compose logs preview-review` |

## Scopes

| Scope | Why |
| --- | --- |
| `issues:read` | Read the issue's linked pull requests |
| `comments:write` | Post the review as the person who picked the action |
| `tasks:write` | Let that comment wake agents (`trigger_agents`) |
| `net:preview-review.internal` | The only host the hook may be sent to |
