# EVC Mesh MCP Server

[![Install via Spark](https://spark.entire.vc/badges/evc-mesh-mcp/install.svg)](https://spark.entire.vc/assets/evc-mesh-mcp?utm_source=github&utm_medium=readme)

[Model Context Protocol](https://modelcontextprotocol.io/) (MCP) server for [EVC Mesh](https://github.com/entire-vc/evc-mesh) — a task management platform for coordinating humans and AI agents.

Connects AI agents (Claude Code, Cursor, Cline, OpenClaw, etc.) to EVC Mesh via MCP tools for task management, persistent memory, event publishing, and multi-agent coordination.

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

## Tool Profiles

The MCP server supports two profiles to optimize context window usage:

| Profile | Tools | Context overhead | Best for |
|---------|-------|-----------------|----------|
| **core** | 20 | ~6K tokens (3% of 200K) | Claude Code, Cursor, small-context models |
| **full** | 45 | ~14K tokens (7% of 200K) | Power users, automation agents, admin ops |

Set via `MESH_MCP_PROFILE` environment variable. Default: `full`.

## Configuration

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `MESH_API_URL` | Yes | `http://localhost:8005` | Base URL of the Mesh API |
| `MESH_AGENT_KEY` | Yes (stdio) | — | Agent API key (`agk_...`) |
| `MESH_MCP_PROFILE` | No | `full` | Tool profile: `core` or `full` |
| `MESH_MCP_TRANSPORT` | No | `stdio` | Transport mode: `stdio` or `sse` |
| `MESH_MCP_HOST` | No | `0.0.0.0` | SSE server bind host |
| `MESH_MCP_PORT` | No | `8081` | SSE server bind port |

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
| `/sse` + `/message` | full | All 45 tools (backward compatible) |
| `/core/sse` + `/core/message` | core | 20 essential tools |

Authentication per connection via:
- `Authorization: Bearer agk_...` header
- `X-Agent-Key: agk_...` header
- `?agent_key=agk_...` query parameter

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

## MCP Tools — Core Profile (20)

### ACP & Identity

| Tool | Description |
|------|-------------|
| `heartbeat` | Send heartbeat. Call at session start with status=online |
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

### Communication

| Tool | Description |
|------|-------------|
| `add_comment` | Add comment to a task (markdown) |
| `publish_event` | Publish event + optional memory hint for persistence |

### Memory

| Tool | Description |
|------|-------------|
| `recall` | Search memory by keywords |
| `remember` | Save knowledge (UPSERT by key) |
| `forget` | Delete a memory entry |

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

## MCP Tools — Full Profile (adds 25 more)

### Additional Task Tools

| Tool | Description |
|------|-------------|
| `get_project` | Get project details with statuses and custom fields |
| `create_subtask` | Create subtask under a parent (`status_slug` optional; defaults to the project's default status, not the parent's) |
| `add_dependency` | Add dependency between tasks |
| `checkout_task` | Atomic task lock for multi-agent coordination |
| `release_task` | Release atomic task lock |

### Comments & Artifacts

| Tool | Description |
|------|-------------|
| `list_comments` | List task comments |
| `upload_artifact` | Upload file/code/log to a task |
| `list_artifacts` | List task artifacts |
| `get_artifact` | Get artifact details and download URL |

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

## Deploy checklist (prod server — no CI)

evc-mesh-mcp has no CD pipeline; deploys are manual. **Mandatory order** (per
[CLAUDE-workflow.md §1b Deploy Discipline](https://github.com/entire-vc/evc-mesh/blob/main/CLAUDE-workflow.md)):
`migrate (goose up)` → `binary swap` → `restart`. Never swap the binary before migrations pass.

```bash
# 1. Build for the prod target
GOOS=linux GOARCH=amd64 go build -o evc-mesh-mcp .

# 2. Copy binary to prod
scp evc-mesh-mcp root@prod-host:/opt/evc-mesh-mcp/evc-mesh-mcp.new

# 3. On the prod host: run migrations FIRST, then swap binary
ssh root@prod-host

  # STEP 1 — Run evc-mesh DB migrations (mcp server reads the same DB).
  # goose CLI is not installed on the host — use the official docker image.
  # If this exits non-zero, STOP — do NOT swap the binary.
  DB_URL=$(grep ^DATABASE_URL /opt/evc-mesh/.env.prod | cut -d= -f2-)
  docker run --rm --network host \
    -v /opt/evc-mesh/migrations:/migrations \
    ghcr.io/pressly/goose:latest \
    goose -dir /migrations postgres "$DB_URL" up

  # STEP 2 — Swap binary (only after migrations succeed)
  mv /opt/evc-mesh-mcp/evc-mesh-mcp /opt/evc-mesh-mcp/evc-mesh-mcp.bak.$(date +%Y%m%d-%H%M%S)
  mv /opt/evc-mesh-mcp/evc-mesh-mcp.new /opt/evc-mesh-mcp/evc-mesh-mcp

  # STEP 3 — Restart
  sudo systemctl restart evc-mesh-mcp

  # STEP 4 — Smoke test
  curl -sf http://localhost:8081/health || echo "SMOKE FAILED"
```

> evc-mesh-mcp does not run its own migrations — it relies on the evc-mesh API's
> schema. The goose step above ensures the schema matches before the new binary serves traffic.

## Related

- [evc-mesh](https://github.com/entire-vc/evc-mesh) — Core platform (API + Web UI)
- [evc-mesh-openclaw-skill](https://github.com/entire-vc/evc-mesh-openclaw-skill) — OpenClaw skill (bash scripts)

## License

[MIT](LICENSE)
