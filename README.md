# betterdo-plugin-gitlab

Reference plugin demonstrating the [BetterDo plugin contract](https://github.com/Rohmilchkaese/BetterDo/blob/main/docs/plugin-contract.md).
Mirrors GitLab issues into BetterDo tasks: issue opened → task created,
issue closed → task completed, issue reopened → task uncompleted.

**< 500 LOC** of Go (excluding tests), MIT-licensed, runs as a single
container that bridges GitLab webhooks into the BetterDo REST API.

## Quick start

1. Run BetterDo (from the [main repo](https://github.com/Rohmilchkaese/BetterDo)):

       cd /path/to/BetterDo
       docker compose up -d

2. Run this plugin alongside it:

       cd /path/to/betterdo-plugin-gitlab
       docker compose -f docker-compose.dev.yaml up

3. Visit http://localhost:8180 (BetterDo web UI), log in as
   `demo@betterdo.local` / `demo`, and go to Settings → Connections.
   Subscribe to the `betterdo-plugin-gitlab` plugin.

4. Configure GitLab to deliver issue webhooks to
   `http://<your-host>:8090/webhook`. Or test locally:

       curl -X POST -H 'Content-Type: application/json' \
         -d '{
           "object_kind": "issue",
           "object_attributes": {
             "id": 1,
             "title": "Fix garden timer",
             "url": "https://gitlab.example.com/issues/1",
             "action": "open"
           },
           "user": {"username": "alice"}
         }' \
         http://localhost:8090/webhook

5. Switch to BetterDo's Inbox — a task titled "Fix garden timer" appears.

## Configuration

| Env var | Required | Default | Description |
|---------|----------|---------|-------------|
| `BETTERDO_API_URL` | yes | — | URL of the BetterDo API (e.g. `http://betterdo-api:8080`) |
| `BETTERDO_PLUGIN_GITLAB_USER_MAP` | yes | — | JSON object mapping GitLab usernames → BetterDo user UUIDs |
| `BETTERDO_PLUGIN_PORT` | no | `:8090` | Listen address |
| `BETTERDO_PLUGIN_STATE_DIR` | no | `/tmp` | Directory for the issue→todo state file |

The user map is a one-line JSON object set via env:

    BETTERDO_PLUGIN_GITLAB_USER_MAP='{"alice": "f47ac10b-58cc-4372-a567-0e02b2c3d479"}'

A subscribed user MUST be present in the map — webhooks for an unmapped
user receive 400. Use your BetterDo admin panel or `GET /api/v1/me` to
look up the UUIDs.

## How it works

1. **Startup** → POSTs the manifest to `${BETTERDO_API_URL}/api/v1/plugins/register`,
   stores the returned `bdo_plg_*` token in memory. Logs a WARN if
   `expiresAt` is within the 7-day warn window.
2. **`GET /health`** → unauthenticated, returns `{"status": "ok"}`.
   BetterDo's lifecycle worker (Story 15.3) polls this every 60s.
3. **`POST /webhook`** → parses the GitLab payload, maps the username to
   a BetterDo user UUID, and either creates a task (action=open),
   completes one (action=close), or uncompletes one (action=reopen).
   Issue→todo state lives in `state.json` (single-key file mount).

## Testing

    go test ./...

12 unit tests covering the open / close / reopen / unknown-event /
unknown-user / malformed-JSON / wrong-method paths plus the state
round-trip + health endpoint.

## Deploying as a plugin

The plugin runs as any container. Two recommended topologies:

- **Docker Compose** alongside BetterDo (see `docker-compose.dev.yaml`).
  Joins BetterDo's `plugin-net` network as `external: true`.
- **Kubernetes** with the BetterDo Helm chart's optional plugin
  NetworkPolicy. Set `pluginIsolation.enabled=true` on the chart and
  label your plugin pod with `app.kubernetes.io/component: plugin`.

## Security considerations

- The plugin holds a scoped API token from registration. It does NOT
  hold OIDC credentials and does NOT need Keycloak.
- The plugin runs in network isolation: it can reach the BetterDo API
  but NOT PostgreSQL / Redis / Keycloak / MinIO directly. This is
  STRIDE F-PLG-I-01 enforced by BetterDo's NetworkPolicy / `plugin-net`
  Docker network (Story 15.4).
- Capability scope is `read` + `write` only — the plugin never gets
  `delete`. Operators who don't trust the plugin enough to grant `write`
  can run it with `read` capability and use it as a notification surface
  only.

## License

MIT — see `LICENSE`.

## See also

- [BetterDo Plugin Contract](https://github.com/Rohmilchkaese/BetterDo/blob/main/docs/plugin-contract.md)
- [BetterDo Plugin Developer Guide](https://github.com/Rohmilchkaese/BetterDo/blob/main/docs/plugin-developer-guide.md)
- [BetterDo main repository](https://github.com/Rohmilchkaese/BetterDo)
