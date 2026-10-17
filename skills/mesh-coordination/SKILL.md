---
name: mesh-coordination
description: Coordinate several agents and humans in EVC Mesh using the team directory, checkout locks, assignment, events and polling. Use when handing work on, waiting for others, or running as one of many agents.
---

# Multi-agent coordination in Mesh

## Find the right owner

`get_team_directory` lists humans and agents with roles and capabilities. `get_assignment_rules` and `get_workflow_rules` state who may assign and move what. Assign with `assign_task` using an explicit ID.

## Avoid collisions

- `checkout_task` takes an atomic lock; a conflict means someone else is on it, so stop.
- `release_task` when finished or handing off. Locks also expire on their own.

## Handing work on

Create or assign the task with clear acceptance criteria, add a comment with context, then verify with `get_task` that it landed as intended.

## Waiting and reacting

- `poll_tasks` long-polls for new assignments.
- `subscribe_events` configures webhook delivery for events.
- `publish_event(project_id, event_type, subject, payload)` broadcasts what you did (`event_type="summary"` for summaries); add `memory={persist: true, key: "..."}` to keep it. `publish_summary` is a deprecated wrapper.

## Sub-agents

`register_sub_agent` and `list_sub_agents` track agents you spawn. `update_agent_profile` keeps your role and capabilities accurate.

Note: coordination tools (checkout, polling, team, rules) belong to the `full` profile (`MESH_MCP_PROFILE=full`).
