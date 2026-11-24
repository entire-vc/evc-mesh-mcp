# Checkout fence on result writes

`add_comment`, `upload_artifact`, `create_artifact_upload`, `confirm_upload`
(of an artifact upload), `move_task` and `finish_task` of a card the Runner holds for this agent carry that card's checkout lock. Mesh then writes
only while the lock is live and answers `409` otherwise, writing nothing. The
Runner side is evc-mesh-runner `docs/checkout-fence.md`; the server side is
evc-mesh `internal/handler/result_fence.go`.

## Where the lock comes from

```
<dir>/<agent_id>/<task_id>.json            written by the Runner at the start of every turn
<dir>/<agent_id>/<task_id>.refusals.jsonl  appended here on a fence refusal
```

`<dir>` is `MESH_CHECKOUT_FENCE_DIR`, else `~/.mesh-runner/checkout-fence`.
`<agent_id>` is the session's agent. A short task id (6-12 hex) finds the
card's file only when exactly one file of this agent starts with it.

The file is read again on every write; nothing is cached, so a resumed session
writes with the lock of the turn that resumed it.

| File | Headers |
|------|---------|
| absent, unreadable, or naming no lock | none: the write is exactly as before |
| `checkout_token` set | `X-Mesh-Checkout-Token` |
| no token, `session_id` + `generation` | `X-Mesh-Checkout-Session-Id` + `X-Mesh-Checkout-Generation` |
| finish only | `X-Mesh-Expected-Input-Revision`: the newer of the file's `input_revision` and the last one a `get_task` of the card returned in this process |

The headers go on the same request as the `Idempotency-Key`
(`docs/idempotency.md`), including its re-sends; the key is not changed by them.

## Refusals

On `409 checkout_precondition_failed` or `409 input_revision_changed` one JSON
line is appended to the journal: `attempt_id` and `turn` (copied from the file),
`code`, `at` (RFC3339 UTC), `tool`, `reassigned` (`context.reassigned`),
`input_revision` (`context.input_revision`). The Runner reads it to stop a turn
whose lock is gone.

The tool result starts with the next step:

- lock lost: `the lock of this turn is gone; stop, write nothing more`
- revision changed: `the task changed; run get_task, then finish again`

## Rollback

The Runner's `[runner] checkout_fence = false` stops writing the files, so the
MCP sends no headers. No MCP release is needed.
