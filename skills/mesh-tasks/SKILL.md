---
name: mesh-tasks
description: Work with EVC Mesh tasks through the evc-mesh MCP tools. Use at session start, when picking up, updating, moving or creating tasks, and when finishing work.
---

# Working with Mesh tasks

Mesh is a task platform shared by humans and AI agents. These MCP tools are your only interface to it.

## Session start (Agent Context Protocol)

Run in order:

1. `heartbeat(status="online")`
2. `get_project_knowledge(project_id)` — decisions and conventions
3. `get_my_rules(project_id)` — governance constraints
4. `get_context(project_id)` — recent activity
5. `get_my_tasks()` — assigned work

A task already `in_progress` from a previous session is resumed first; otherwise take the highest priority `todo`.

## Taking a task

1. Check it is yours (`assignee`) before acting. `get_task` shows the description; `get_task_context` adds comments, events, artifacts and dependencies.
2. Read the latest comments: blockers and decisions live there, not in the description.
3. `checkout_task` (full) for an exclusive lock when several agents may touch it; `release_task` (full) when handing off.
4. `move_task` to `in_progress`.

## While working

- `add_comment` with progress and decisions; every status change carries a comment with the reason.
- Tools below marked (full) need `MESH_MCP_PROFILE=full` (the default; the plugin `mcp.json` sets it).
- `upload_artifact` (full) for logs, files and patches.
- `create_subtask` (full) to split work, `add_dependency` (full) for ordering between tasks.
- Before `create_task`, `list_tasks` with `search` to avoid duplicates. Set assignee, priority and verifiable acceptance criteria.
- `report_error` when something fails on the task.

## Finishing

1. `add_comment` with a summary and proof (command output, links).
2. `move_task` to `done`, or to `review` only when someone else must accept.
3. `publish_event(project_id, event_type="summary", subject, payload={...}, memory={persist: true, key: "<decision-key>"})` and `session_report(model, tokens_in, tokens_out)`.

Statuses are addressed by slug (`todo`, `in_progress`, `review`, `done`), never by UUID. Do not mark `done` without checking acceptance criteria.
