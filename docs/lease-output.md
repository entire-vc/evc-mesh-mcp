# Lease capabilities at the MCP boundary

REST checkout responses and task objects contain `checkout_token`, a bearer
capability used internally for extend/release. MCP JSON results omit it recursively,
including `full=true`, cold/delta assigned-task snapshots, typed values and nested
cards. Equivalent `lease_token` and `fencing_token` keys are also excluded. Current
REST models expose only `checkout_token`; token usage counters and ordinary prose
are unaffected. `full=true` means complete non-capability fields.

All JSON tool results pass through `jsonResult`, which filters a private wire copy
before creating MCP text content. No handler emits separate structured content;
tests serialize the entire `CallToolResult`, covering both wire slots. Internal REST
objects and snapshot caches remain unchanged. Numeric generations retain int64
precision. Owner, state, session/request IDs, generation and expiry are retained
where the corresponding view already exposes them.

| Read path | Task-bearing payload |
| --- | --- |
| `get_task` | Default/full, changed-since task, included dependencies/subtasks |
| `get_task_context` | Task, dependencies, subtasks, recurring previous instance |
| `list_tasks` | Project/workspace compact/full task pages |
| `get_my_tasks` | Full, cold, delta and unchanged assigned-task snapshots |
| `poll_tasks` | Default/full assigned tasks |
| `get_recurring_history` | Recurring task instances |
| `get_context` | Nested task objects in event payloads |

Other JSON tools, including task mutations, dependency/recurring operations and
knowledge/document reads with nested objects, use the same boundary with no
per-tool exemption. Checkout caches its capability before serialization;
extend/release consume that cache using only the caller's `task_id`. REST writer
protocols and generation/session/owner guards are unchanged. Restarting an MCP
process still loses its cache; this change does not introduce recovery by chat.

## Verification and installed targets

`go test ./internal/mcp -run 'TestLeaseOutput|TestCheckoutRelease|TestExtendCheckout'`
tests nested/typed/RawMessage filtering, both result slots, metadata, immutable
inputs, snapshot deltas and capability forwarding through checkout/extend/release.

The server runs as a local stdio binary or as a long-lived SSE / Streamable HTTP
service (for the hosted product: `https://mesh.entire.host/mcp`). Both transports
use the same tool serialization. Replace binaries atomically;
existing stdio processes retain the previous inode until restarted.

The live probe creates an owned `no-intake` canary, checks a nonempty REST capability
as positive control, reads it through installed MCP without logging values, verifies
metadata and checkout/extend/release, and cancels the canary in cleanup:

```sh
# Internal deployment acceptance probe is omitted from this public snapshot.
```

Run the probe against the previous release first as a negative control: it must
fail there. Credentials come from a `MESH_AGENT_KEY` entry in a local env file
(`--credential-file`). Reports contain booleans/counts/fixture IDs,
never capabilities or raw tool responses. No permanent credential rotation occurs.
