# EVC Mesh MCP Server

<!-- mcp-name: io.github.entire-vc/evc-mesh-mcp -->

[![Install via Spark](https://spark.entire.vc/badges/evc-mesh-mcp/install.svg)](https://spark.entire.vc/assets/evc-mesh-mcp?utm_source=github&utm_medium=readme)

[Model Context Protocol](https://modelcontextprotocol.io/) (MCP) server for [EVC Mesh](https://github.com/entire-vc/evc-mesh) — a task management platform for coordinating humans and AI agents.

Connects AI agents (Claude Code, Cursor, Cline, OpenClaw, etc.) to EVC Mesh via MCP tools for task management, persistent memory, event publishing, and multi-agent coordination.

**This is the actively developed copy.** [`evc-mesh`](https://github.com/entire-vc/evc-mesh) also ships an MCP server (`./cmd/mcp`, same `internal/mcp` tool set) that it builds and deploys itself — the two exist because Go's `internal/` visibility rules mean one repo can't import the other's package, not because they're meant to diverge. New tools and fixes land here first.

## Prerequisites

- Go 1.22+
- Running EVC Mesh instance
- Agent registered in Mesh with an API key (`agk_...`)

## Installation

```bash
go install github.com/entire-vc/evc-mesh-mcp@latest
```

Or build from source:

```bash
git clone https://github.com/entire-vc/evc-mesh-mcp.git
cd evc-mesh-mcp
go build -o evc-mesh-mcp .
```

### Docker

```bash
docker run -i --rm \
  -e MESH_API_URL \
  -e MESH_AGENT_KEY \
  ghcr.io/entire-vc/evc-mesh-mcp
```

`-i` is required — the server speaks MCP over stdio, and Docker only wires up
stdin when the container runs interactively. Add `-e MESH_MCP_PROFILE=core`
to switch profiles (see [Tool Profiles](#tool-profiles) below). The image is
published for `linux/amd64` and `linux/arm64` from `Dockerfile` in this repo
on every tagged release (`docs/RELEASING.md`).

## Tool Profiles

The MCP server supports two profiles to optimize context window usage:

| Profile | Tools | Context overhead | Best for |
|---------|-------|-----------------|----------|
| **core** | 25 | ~8K tokens (4% of 200K) | Claude Code, Cursor, small-context models |
| **full** | 63 | ~18K tokens (9% of 200K) | Power users, automation agents, admin ops |

Set via `MESH_MCP_PROFILE` environment variable. Default: `full`.

## Configuration

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `MESH_API_URL` | Yes | `http://localhost:8005` | Base URL of the Mesh API |
| `MESH_AGENT_KEY` | Yes (stdio) | — | Agent API key (`agk_...`) |
| `MESH_MCP_PROFILE` | No | `full` | Tool profile for stdio: `core` or `full` (SSE serves both) |
| `MESH_MCP_TRANSPORT` | No | `stdio` | Transport mode: `stdio` or `sse` |
| `MESH_MCP_HOST` | No | `0.0.0.0` | SSE server bind host |
| `MESH_MCP_PORT` | No | `8081` | SSE server bind port |
| `MESH_MCP_AUTH_FAIL_RPM` | No | `20` | SSE mode: per-IP budget for authentication attempts against a not-yet-cached agent key on `/sse`, `/core/sse`, `/mcp`, `/mcp/core`. Over budget → `429` without calling Mesh API. `0` disables. |
| `MESH_MCP_SESSION_CACHE_TTL_MIN` | No | `15` | SSE mode: how long a successful authentication is trusted before the key is re-checked — bounds how long a revoked key keeps working without a restart. |
| `MESH_MCP_AUTH_FAIL_CACHE_SEC` | No | `30` | SSE mode: how long a failed authentication (bad/unknown key) is remembered, so repeating the same bad key doesn't call Mesh API every request. |
| `MESH_MCP_PUBLIC_URL` | No | — | SSE mode: the URL the MCP root is reachable at from outside, e.g. `https://mesh.example.com/mcp`. Used for the absolute SSE endpoint and as the OAuth resource URL (see [OAuth](#oauth-remote-connectors)). Unset: derived from each request's host, which is only right when clients reach this server directly — behind a proxy set it, or the derived core URL (`/core`) will not match the public one (`/mcp/core`). |
| `MESH_MCP_OAUTH_ISSUER` | No | origin of `MESH_MCP_PUBLIC_URL` | SSE mode: the OAuth authorization server named in the protected-resource metadata — your Mesh instance's public origin. Set it only if the MCP server is served from a different origin than the Mesh API. |
| `MESH_MCP_OAUTH_CACHE_TTL_SEC` | No | `60` | SSE mode: how long a verified OAuth access token is trusted before Mesh API is asked again. Deliberately much shorter than the agent-key TTL so a revoked grant stops working within about a minute. |
| `MESH_MCP_DECIDER_USERNAME` | No | — | Username recorded as `decided_by` when `record_owner_decision` answers a gated task. Unset: the workspace owner. |
| `MESH_MCP_LEGACY_TOOL_ALIASES` | No | off | `1` also registers the tools' earlier names, for deployments whose callers still use them. Leave off for new installs. |

### Running without credentials

In stdio mode the server also starts when `MESH_AGENT_KEY` is not set. It then
answers `initialize` and `tools/list` as usual, and every tool call returns
instructions for setting `MESH_API_URL` and `MESH_AGENT_KEY`. This lets MCP
clients and catalogs inspect the tool list before you have a key. If a key is
set but authentication fails at startup (API unreachable, key rejected), the
server keeps running: tools are listed, and each call retries authentication
and returns the reason until it succeeds.

### Tool annotations

Every tool declares the MCP hints `readOnlyHint`, `destructiveHint`,
`idempotentHint` and `openWorldHint`, so clients can tell read-only tools
(`get_*`, `list_*`, `recall`, `search_docs`, …) from ones that change or
remove data (`update_*`, `move_task`, `forget`, …).

### Client metrics

Each `initialize` is logged with the client's `clientInfo.name` and version,
and counted in the Prometheus metric `mesh_mcp_initialize_total{client,profile}`
(exposed on `/metrics` in SSE mode; client names are normalised and capped).

### Claude Code (stdio mode)

Add to your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "evc-mesh": {
      "command": "evc-mesh-mcp",
      "env": {
        "MESH_API_URL": "https://your-mesh-instance.example.com",
        "MESH_AGENT_KEY": "agk_your-workspace_your-key",
        "MESH_MCP_PROFILE": "core"
      }
    }
  }
}
```

### Cursor

Add to Cursor MCP settings (Settings → MCP Servers):

```json
{
  "evc-mesh": {
    "command": "evc-mesh-mcp",
    "env": {
      "MESH_API_URL": "https://your-mesh-instance.example.com",
      "MESH_AGENT_KEY": "agk_your-workspace_your-key",
      "MESH_MCP_PROFILE": "core"
    }
  }
}
```

### SSE Mode (multi-agent, shared server)

For connecting multiple agents through a shared MCP endpoint:

```bash
MESH_API_URL=https://your-mesh-instance.example.com \
MESH_MCP_PORT=8081 \
evc-mesh-mcp --transport sse
```

SSE mode serves **two profiles simultaneously** on different paths:

| Path | Profile | Description |
|------|---------|-------------|
| `/sse` + `/message` | full | All 63 tools (backward compatible) |
| `/core/sse` + `/core/message` | core | 25 essential tools |

The same process also serves the **Streamable HTTP** transport (stateless, one
agent key per request, sent in the `Authorization: Bearer` or `X-Agent-Key`
header — the query parameter is refused there):

| Path | Profile |
|------|---------|
| `/mcp` | full |
| `/core` | core |

Authentication per connection via:
- `Authorization: Bearer agk_...` header
- `X-Agent-Key: agk_...` header
- `?agent_key=agk_...` query parameter (SSE connect only)
- `Authorization: Bearer mot_...` — an OAuth access token issued by your Mesh instance (see below); the header only, never the query string, and on the Streamable HTTP endpoints only (SSE connections need an agent key)

### OAuth (remote connectors)

Clients that sign users in with OAuth — remote-connector directories, MCP
Inspector, editors that follow the MCP authorization spec — connect to the
Streamable HTTP endpoints without a pre-shared key. Your Mesh instance is the
authorization server (dynamic client registration, PKCE, user consent); this
server is the resource server and does two things:

- **Challenges.** A request with no credential, or with an OAuth access token
  Mesh API rejects (expired, revoked, never issued), gets `401` and

  ```
  WWW-Authenticate: Bearer resource_metadata="https://mesh.example.com/.well-known/oauth-protected-resource/mcp", scope="mesh"
  ```

  which is where a client starts the authorization flow. A rejected agent key
  keeps answering `403`. Only a verdict from Mesh API (a `4xx` other than
  `408`/`429`) makes a token invalid: if Mesh API cannot be reached or answers
  with an error that says nothing about the token (`5xx`, `429`), the answer is
  `503` with `Retry-After`, so a valid token is not thrown away over an outage.
- **Serves the metadata** ([RFC 9728](https://www.rfc-editor.org/rfc/rfc9728))
  at the well-known path derived from each endpoint's URL:

  | Endpoint | Metadata |
  |----------|----------|
  | `https://mesh.example.com/mcp` (full) | `/.well-known/oauth-protected-resource/mcp` |
  | `https://mesh.example.com/mcp/core` (core) | `/.well-known/oauth-protected-resource/mcp/core` |

  ```json
  {
    "resource": "https://mesh.example.com/mcp",
    "authorization_servers": ["https://mesh.example.com"],
    "scopes_supported": ["mesh"],
    "bearer_methods_supported": ["header"]
  }
  ```

  `resource` is `MESH_MCP_PUBLIC_URL` (trailing slash, query and fragment
  removed; `/core` appended for the core profile), so set it to the URL your
  users paste into the client.
  The authorization server defaults to that URL's origin; override it with
  `MESH_MCP_OAUTH_ISSUER`.

An OAuth token acts as a connector agent in the workspace the user chose when
they granted access, with that agent's permissions — the same tools, the same
permission model as an agent key. Verified tokens are cached for a minute (`MESH_MCP_OAUTH_CACHE_TTL_SEC`). The
per-IP budget (`MESH_MCP_AUTH_FAIL_RPM`) is spent by *rejected* tokens, not by
verifications, so many users behind one address are not throttled by their own
token refreshes. The token is not audience-bound: any valid access token from
your Mesh instance is accepted, which is the intent while the authorization
server and this server belong to the same deployment. Agent keys (`agk_...`) in `Authorization` or
`X-Agent-Key` work exactly as before, and stdio mode is unaffected.

Your reverse proxy must send `/.well-known/oauth-protected-resource*` to this
server instead of the web app's catch-all: a single-page app answers every
unknown path with `200 text/html`, which a client cannot tell from missing
metadata.

## Agent Context Protocol (ACP)

At session start, follow these 5 steps in order:

```
1. heartbeat(status="online")              → register as alive
2. get_project_knowledge(project_id)       → load accumulated decisions & conventions
3. get_my_rules(project_id)                → understand constraints
4. get_context(project_id)                 → see recent activity + project knowledge
5. get_my_tasks()                          → check assigned work
```

At session end:
```
publish_event(type="summary", memory={persist: true})  → broadcast + persist
session_report(model, tokens_in, tokens_out)           → report metrics
```

## MCP Tools — Core Profile (25)

### ACP & Identity

| Tool | Description |
|------|-------------|
| `heartbeat` | Send heartbeat. Call at session start with status=online. Response includes `mesh_version` (the running binary's build git-SHA, or `"dev"` for an unpinned local build) — cheap way to check whether a fix has actually reached the installed binary without shelling out to the host. |
| `get_project_knowledge` | Get ALL permanent knowledge (decisions, conventions). ACP Step 2 |
| `get_my_rules` | Get ALL governance rules (workflow + assignment). ACP Step 3 |
| `get_context` | Get recent activity + project knowledge. ACP Step 4 |
| `get_my_tasks` | Get assigned tasks. ACP Step 5 |

### Task Management

| Tool | Description |
|------|-------------|
| `list_projects` | List workspace projects |
| `list_tasks` | List tasks with filters (status, priority, assignee, search) |
| `get_task` | Get task details with optional comments/artifacts/deps |
| `create_task` | Create a new task |
| `update_task` | Update task fields |
| `move_task` | Change task status using slugs |
| `assign_task` | Assign/unassign a task |
| `get_task_context` | Get everything about a task in one call |
| `add_vcs_link` | Link a task to a pull request, commit or branch |

### Communication

| Tool | Description |
|------|-------------|
| `add_comment` | Add comment to a task (markdown). Response includes a `delivery` array per `@`-mention reporting whether it actually reached the recipient (task queue/notification) or was skipped/failed and why |
| `publish_event` | Publish event + optional memory hint for persistence |

### Memory

| Tool | Description |
|------|-------------|
| `recall` | Search memory by keywords |
| `remember` | Save knowledge (UPSERT by key) |
| `forget` | Delete a memory entry |
| `recall_with_graph` | Search memory, expanding results through the knowledge graph |
| `set_project_knowledge` | Write a structured project fact (upsert by key) |
| `get_canonical_updates` | Fetch canonical decisions recorded since a given time |
| `record_owner_decision` | Record a decision by the workspace owner as canonical project knowledge |

#### What `recall` guarantees about its result

**`limit` is a hard bound.** The response never contains more than `limit` items,
and `total` always equals the number of items actually returned. Nothing is added
to the page after it has been sized — not pinned rows, not graph-expanded
neighbours.

**Rows that fail `scope`/`tags`/`tags_any` are dropped, never returned unmarked.**
This holds regardless of how a row reached the result: ordinary retrieval, pinning,
or graph expansion. A pinned row is exempt from *ranking*, not from *eligibility* —
"pinned" means "do not let ranking bury this", not "show this to a caller who asked
for a different scope".

**Graph neighbours are marked and bounded.** With `RECALL_GRAPH_ENABLED=true`,
`recall` also runs a knowledge-graph expansion and folds in `hop > 0` neighbours,
each carrying `graph_boost: true` and `provenance: via:graph`. They occupy at most
`limit/4` of the page (at least 1 when `limit >= 2`, none when `limit < 2`) and take
its **tail** slots, displacing the weakest retrieval hits rather than being appended
on top. When expansion returns nothing usable, the page is exactly the base result —
the reserve is a ceiling, not a quota. `graph_boost_count` reports how many slots
were actually spent.

The reserve exists because base hits carry `score` (RRF across the retrieval arms)
and neighbours carry `composite_score` from a separate traversal — different fields
on different scales. Sorting the union on a common key does not balance them; in
practice every observed neighbour ranks below every base hit, so a naive merge-sort
would silently disable graph boost. The reserve makes that trade explicit and
tunable.

**Presets never overrule you.** `recall` classifies the query and may apply a
profile (e.g. multi-session widens the page). A profile only fills in parameters you
did not supply; an explicit `limit` always wins.

### Utility

| Tool | Description |
|------|-------------|
| `report_error` | Report an error on a task |
| `session_report` | Report session metrics (model, tokens, cost) |

## MCP Tools — Full Profile (adds 38 more, 63 total)

### Additional Task Tools

| Tool | Description |
|------|-------------|
| `get_project` | Get project details with statuses and custom fields |
| `create_subtask` | Create subtask under a parent (`status_slug` optional; defaults to the project's default status, not the parent's) |
| `add_dependency` | Add dependency between tasks |
| `checkout_task` | Atomic task lock for multi-agent coordination |
| `release_task` | Release atomic task lock |
| `extend_checkout` | Extend an existing task lock for longer-running work |
| `set_human_gate` | Freeze a task until a named person answers a recorded question |
| `clear_human_gate` | Release a human gate |

### Comments & Artifacts

| Tool | Description |
|------|-------------|
| `list_comments` | List task comments |
| `upload_artifact` | Upload file/code/log to a task |
| `list_artifacts` | List task artifacts |
| `get_artifact` | Get artifact details (`download_path`; bytes via the two-step download below) |

### Downloading an artifact

Downloading an artifact is two GETs. Step 1: GET <base>/api/v1/artifacts/<id>/download with header X-Agent-Key: <your agent key> -> 200 JSON {"url": "<presigned URL>"}. Step 2: GET that url with NO headers -> 200, the file bytes. Pitfalls: on step 1 only X-Agent-Key is accepted (X-API-Key and Authorization: Bearer give 401); on step 2 any extra header, Authorization in particular, breaks the presigned signature (400). The artifact's download_path is step 1's path. Never fetch browser_only_url with an agent key: it is a human page and answers 401 by design.

### Event Bus

| Tool | Description |
|------|-------------|
| `publish_summary` | Publish work summary (convenience wrapper) |
| `subscribe_events` | Configure webhook delivery for events |
| `poll_tasks` | Long-poll for new task assignments |

### Agent & Team

| Tool | Description |
|------|-------------|
| `register_sub_agent` | Register a sub-agent |
| `list_sub_agents` | List sub-agents (optionally recursive) |
| `get_team_directory` | Get workspace team directory |
| `update_agent_profile` | Update agent role, capabilities, profile |

### Governance & Config

| Tool | Description |
|------|-------------|
| `get_project_rules` | Get all project rules |
| `get_assignment_rules` | Get assignment rules |
| `get_workflow_rules` | Get workflow rules with caller permissions |
| `import_workspace_config` | Import workspace config from YAML |
| `export_workspace_config` | Export workspace config as YAML |

### Recurring Tasks

| Tool | Description |
|------|-------------|
| `create_recurring_task` | Create recurring task schedule |
| `list_recurring_schedules` | List recurring schedules |
| `get_recurring_history` | Get instance history for a schedule |
| `trigger_recurring_now` | Trigger next instance immediately |
| `update_recurring_schedule` | Change or deactivate a recurring schedule |
| `delete_recurring_schedule` | Delete a recurring schedule (existing instances stay) |

### Documents & Knowledge

| Tool | Description |
|------|-------------|
| `list_docs` | List a project's documents (metadata only) |
| `get_doc` | Read a document (outline by default, body on request) |
| `search_docs` | Full-text search across a project's documents |
| `create_doc` | Create a document |
| `update_doc` | Edit a document (optimistic concurrency via `base_version`) |
| `comment_doc` | Comment on a document or a quoted passage |
| `list_doc_comments` | Read a document's comment threads |
| `get_canonical` | Query curated facts and decisions for a topic |

## Architecture

```
AI Agent (Claude Code / Cursor / Cline / OpenClaw)
    ↕ MCP (stdio or SSE)
EVC Mesh MCP Server (core or full profile)
    ↕ REST API (HTTP)
EVC Mesh API Server
    ↕
PostgreSQL / Redis / NATS / S3
```

The MCP server is a lightweight proxy — it translates MCP tool calls into REST API requests. No direct database access needed.

## Running the shared HTTP server

To serve several agents from one process, run the server in SSE mode next to
your Mesh API (the same image works: `docker run -e MESH_MCP_TRANSPORT=sse
-e MESH_API_URL=... -p 8081:8081 ghcr.io/entire-vc/evc-mesh-mcp`) and put it
behind your reverse proxy. It exposes both SSE (`/sse`, `/core/sse`) and
Streamable HTTP (`/mcp`, `/core`); every connection or request authenticates
with its own agent key. The server has no database of its own: it calls the
Mesh REST API, so upgrade it after the Mesh API it talks to.

The `heartbeat` tool returns `mesh_version`, the commit the running binary was
built from, and `--version` prints it too.

## Related

- [evc-mesh](https://github.com/entire-vc/evc-mesh) — Core platform (API + Web UI)
- [evc-mesh-openclaw-skill](https://github.com/entire-vc/evc-mesh-openclaw-skill) — OpenClaw skill (bash scripts)

## License

[MIT](LICENSE)
