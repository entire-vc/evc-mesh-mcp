# EVC Mesh MCP Server

[![Install via Spark](https://spark.entire.vc/badges/evc-mesh-mcp/install.svg)](https://spark.entire.vc/assets/evc-mesh-mcp?utm_source=github&utm_medium=readme)

[Model Context Protocol](https://modelcontextprotocol.io/) (MCP) server for [EVC Mesh](https://github.com/entire-vc/evc-mesh) — a task management platform for coordinating humans and AI agents.

Connects AI agents (Claude Code, Cline, etc.) to EVC Mesh via MCP tools for task management, event publishing, artifact uploads, and multi-agent coordination.

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

## Configuration

The MCP server connects to the Mesh REST API. Only two environment variables are needed:

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `MESH_API_URL` | Yes | `http://localhost:8005` | Base URL of the Mesh API |
| `MESH_AGENT_KEY` | Yes (stdio) | — | Agent API key (`agk_...`) |
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
      "args": ["--transport", "stdio"],
      "env": {
        "MESH_API_URL": "https://your-mesh-instance.example.com",
        "MESH_AGENT_KEY": "agk_your-workspace_your-key"
      }
    }
  }
}
```

If running from source:

```json
{
  "mcpServers": {
    "evc-mesh": {
      "command": "go",
      "args": ["run", "."],
      "cwd": "/path/to/evc-mesh-mcp",
      "env": {
        "MESH_API_URL": "https://your-mesh-instance.example.com",
        "MESH_AGENT_KEY": "agk_your-workspace_your-key"
      }
    }
  }
}
```

### SSE Mode (multi-agent)

For connecting multiple agents through a shared MCP endpoint:

```bash
MESH_API_URL=https://your-mesh-instance.example.com \
MESH_MCP_PORT=8081 \
evc-mesh-mcp --transport sse
```

SSE mode supports per-connection authentication via:
- `Authorization: Bearer agk_...` header
- `X-Agent-Key: agk_...` header
- `?agent_key=agk_...` query parameter

## MCP Tools (33)

### Project & Task Management

| Tool | Description |
|------|-------------|
| `list_projects` | List workspace projects |
| `get_project` | Get project details with statuses and custom fields |
| `list_tasks` | List tasks with filters (status, priority, assignee, search) |
| `get_task` | Get task details |
| `create_task` | Create a new task |
| `update_task` | Update task fields |
| `move_task` | Change task status |
| `create_subtask` | Create subtask under a parent |
| `add_dependency` | Add dependency between tasks |
| `assign_task` | Assign or unassign a task |

### Comments & Artifacts

| Tool | Description |
|------|-------------|
| `add_comment` | Add comment to a task |
| `list_comments` | List task comments |
| `upload_artifact` | Upload file/code/log to a task |
| `list_artifacts` | List task artifacts |
| `get_artifact` | Get artifact details and download URL |

### Event Bus

| Tool | Description |
|------|-------------|
| `publish_event` | Publish event to project event bus |
| `publish_summary` | Publish a work summary event |
| `get_context` | Get aggregated project context |
| `get_task_context` | Get enriched task context (task + deps + comments + artifacts) |
| `subscribe_events` | Subscribe to project events |

### Agent Lifecycle

| Tool | Description |
|------|-------------|
| `heartbeat` | Send agent heartbeat (online/busy/error) |
| `get_my_tasks` | List tasks assigned to calling agent |
| `report_error` | Report an error on a task |

### Agent Hierarchy

| Tool | Description |
|------|-------------|
| `register_sub_agent` | Register a sub-agent under the calling agent |
| `list_sub_agents` | List sub-agents (optionally recursive) |

### Governance

| Tool | Description |
|------|-------------|
| `get_my_rules` | Get governance rules applicable to calling agent |
| `get_project_rules` | Get governance rules for a project |

### Team & Rules

| Tool | Description |
|------|-------------|
| `get_team_directory` | Get workspace team directory (agents + humans with profiles) |
| `get_assignment_rules` | Get effective assignment rules for a project (workspace + project merged) |
| `get_workflow_rules` | Get workflow rules for a project including caller permissions |
| `update_agent_profile` | Update calling agent's role, capabilities, and profile fields |
| `import_workspace_config` | Import workspace configuration from YAML |
| `export_workspace_config` | Export workspace configuration as YAML |

## Architecture

```
AI Agent (Claude Code / Cline / etc.)
    ↕ MCP (stdio or SSE)
EVC Mesh MCP Server
    ↕ REST API (HTTP)
EVC Mesh API Server
    ↕
PostgreSQL / Redis / NATS / S3
```

The MCP server is a lightweight proxy — it translates MCP tool calls into REST API requests. No direct database, cache, or message bus access needed.

## Related

- [evc-mesh](https://github.com/entire-vc/evc-mesh) — Core platform (API + Web UI)
- [evc-mesh-openclaw-skill](https://github.com/entire-vc/evc-mesh-openclaw-skill) — OpenClaw skill (bash scripts)

## License

[MIT](LICENSE)
