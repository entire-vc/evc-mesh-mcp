# Installing the EVC Mesh MCP server (for AI assistants)

Follow these steps exactly; they are written for an assistant setting this
server up on the user's machine.

## 1. Ask the user for two values

- `MESH_API_URL` — base URL of the user's Mesh instance, e.g. `https://mesh.example.com`
  (no `/api` suffix).
- `MESH_AGENT_KEY` — the agent's API key from that instance (starts with `agk_`).

If the user has no key yet, install anyway: the server starts without one,
lists its tools, and every tool call explains how to configure it.

## 2. Install the binary

Pick one:

- **Go 1.22+ available:** `go install github.com/entire-vc/evc-mesh-mcp@latest`
  (the binary lands in `$(go env GOPATH)/bin/evc-mesh-mcp`).
- **Docker available:** no install step; use the image `ghcr.io/entire-vc/evc-mesh-mcp`.
- **Neither:** download the archive for the user's OS and CPU from the latest
  GitHub release (`https://github.com/entire-vc/evc-mesh-mcp/releases/latest`),
  unpack it and use the absolute path to `evc-mesh-mcp`.

## 3. Add it to the MCP settings

Use the absolute path to the binary. The tool profile is set with
`MESH_MCP_PROFILE` (`core` = 25 everyday tools, `full` = 63 tools; default `full`).

```json
{
  "mcpServers": {
    "evc-mesh": {
      "command": "/absolute/path/to/evc-mesh-mcp",
      "env": {
        "MESH_API_URL": "https://mesh.example.com",
        "MESH_AGENT_KEY": "agk_...",
        "MESH_MCP_PROFILE": "core"
      }
    }
  }
}
```

Docker variant:

```json
{
  "mcpServers": {
    "evc-mesh": {
      "command": "docker",
      "args": ["run", "-i", "--rm", "-e", "MESH_API_URL", "-e", "MESH_AGENT_KEY", "-e", "MESH_MCP_PROFILE", "ghcr.io/entire-vc/evc-mesh-mcp"],
      "env": {
        "MESH_API_URL": "https://mesh.example.com",
        "MESH_AGENT_KEY": "agk_...",
        "MESH_MCP_PROFILE": "core"
      }
    }
  }
}
```

No command-line arguments are needed: the server speaks MCP over stdio by default.

## 4. Check it works

Call the `get_my_tasks` tool. With a valid key it returns the tasks assigned to
the agent; with a wrong key it returns `Unauthorized: Invalid agent API key`.
