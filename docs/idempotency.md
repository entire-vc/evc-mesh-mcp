# Idempotency-Key on result writes

`add_comment`, `upload_artifact` and `finish_task` send an `Idempotency-Key`
header. A write whose answer was lost (timeout, reset, restart) can then be sent
again without writing twice. The server side is evc-mesh `docs/idempotency.md`
(the comments, artifacts and finish routes; key scoped to caller + method +
path; kept 24 h).

## What the client does

| Situation | Client |
|-----------|--------|
| Answer lost (transport error) | Sends again with the same key and body after 1 s, then 3 s. |
| `409 idempotency_request_in_progress` | Waits `Retry-After` (never less), sends again with the same key; at most 3 times. A `Retry-After` above 30 s is returned to the agent instead. |
| `409 idempotency_key_reused` | The write already exists: the tool returns `idempotent_reused: true`, no error. |
| `Idempotent-Replayed: true` | The tool result carries `idempotent_replayed: true`. |

Other turn commands (`wait_for`, `continue_next`, ...) send no key.

## The key

`mcp-` + sha256(caller | scope | tool | task | sha256(arguments) | n)

- **caller**: a digest of the credential and the API base URL.
- **scope**: the runner checkout of the attempt (`MESH_RUNNER_CHECKOUT_SESSION`
  + `MESH_RUNNER_CHECKOUT_GENERATION`, exported by mesh-runner). Without them
  it is a random id of the process.
- **n**: the same arguments again within the window (default 10 min since
  the last call with them) keep `n`, so the server replays instead of writing.
  After the window they are a new write (`n+1`).

So an agent that repeats a tool call after a lost answer (same card, tool,
arguments and runner checkout) gets the first write back. A restarted MCP
process under the same checkout starts at `n=0` and also lands on the first
write. Outside mesh-runner a restarted process has a new scope and a new key.

Multipart fields are written in sorted order, so a rebuilt upload has the same
parts and therefore the same server fingerprint.

## Settings

| Variable | Default | Effect |
|----------|---------|--------|
| `MESH_MCP_IDEMPOTENCY_KEYS` | on | `0` / `false` / `off` / `no`: no header, no extra sends, exactly the behaviour before keys. Read on every call. |
| `MESH_MCP_IDEMPOTENCY_WINDOW_SECS` | `600` | Repeat window for `n`, 1 to 86400 s. |

Keys start with `mcp-`, so the server's key store tells MCP writes apart from
the Runner's (uuid5) keys.
