# Deploying this binary to mesh-vm

`https://mesh.entire.host/mcp` is served by `mesh-mcp.service` on **mesh-vm**
(`10.10.10.10`, reachable only through the hel01 edge). Today that service runs
a binary built from a *second copy* of this MCP server that lives in
`evc-mesh/cmd/mcp`; the two copies have drifted by 37 functions, all in one
direction — this repository is ahead. Mesh task `#3bc9f59d` removes the copy,
and this document is the delivery half of that.

Nothing here has cut over yet. `deploy-mesh-vm.yml` exists, is runnable, and
defaults to a dry run. The switch is task `#2bfbff4c`.

## The parts

| | |
|---|---|
| `.github/workflows/deploy-mesh-vm.yml` | builds `linux/amd64`, ships it, calls the remote script |
| `scripts/mesh-mcp-remote-deploy.sh` | everything that happens **on** mesh-vm: anchor, swap, restart, smoke, rollback |
| `scripts/mesh-mcp-deploy-drill.sh` | exercises the remote script against a scratch directory; runs on every CI build |

The remote script is re-uploaded on every run, so the code executing on prod is
the code in that commit. A host-side copy cannot drift from the reviewed one.

## Running it

The workflow is **dispatch-only** and has three modes:

```bash
gh workflow run deploy-mesh-vm.yml --repo entire-vc/evc-mesh-mcp -f mode=dry-run
gh workflow run deploy-mesh-vm.yml --repo entire-vc/evc-mesh-mcp -f mode=deploy
gh workflow run deploy-mesh-vm.yml --repo entire-vc/evc-mesh-mcp -f mode=rollback
```

`dry-run` verifies the uploaded artifact, prints the exact plan including the
anchor name it would write, then deletes the artifact. It touches neither the
live binary nor the service.

There is deliberately **no `push:` trigger**. Until the cutover, evc-mesh's
`deploy-backend.yml` also writes `/opt/evc-mesh/bin/mesh-mcp` on every backend
merge, and GitHub's `concurrency` groups are per-repository — no group can
serialise two repositories. Adding

```yaml
on:
  push:
    branches: [main]
```

is the one-line change that completes the cutover, and it belongs in the same
change that removes the mesh-mcp build and swap steps from `deploy-backend.yml`
(task `#e85e4e05`). While both pipelines exist, a `deploy` or `rollback` run
refuses to start if an evc-mesh backend deploy is in flight — evc-mesh is a
public repository, so its run list is readable without a token, and the check
holds the deploy if the API cannot be read rather than assuming it is clear.

## Rollback

Every `deploy` copies the outgoing binary to
`/opt/evc-mesh/bin/mesh-mcp.rollback-<UTC>-<NN>-mcprepo` **before** the swap and
refuses to swap if that copy cannot be written. The ten most recent are kept.

Pruning is scoped to the `-mcprepo` suffix: evc-mesh's pipeline writes
`-ci-auto` anchors into the same directory and this script never deletes them.
(Those are worth a separate look — 99 `-ci-auto` anchors plus 3 older hand-made
ones, 1.3 GB in that directory, when this was written. `deploy-backend.yml`
creates them and prunes nothing.)

A failed smoke test rolls back on its own and *then* fails the job. To roll back
by hand:

```bash
gh workflow run deploy-mesh-vm.yml --repo entire-vc/evc-mesh-mcp -f mode=rollback
# or, on the host:
ssh mesh-vm '/opt/evc-mesh/bin/mesh-mcp-remote-deploy.sh rollback'
ssh mesh-vm '/opt/evc-mesh/bin/mesh-mcp-remote-deploy.sh status'
```

`rollback` restores the newest anchor and asserts afterwards that the running
process really executes those bytes. `ANCHOR=<path>` picks a specific one.

## What the smoke test asserts

A 200 is not evidence a feature works, so every assertion is about content, and
three of them can tell this binary from the `evc-mesh/cmd/mcp` one:

| assertion | this binary | `evc-mesh/cmd/mcp` |
|---|---|---|
| `mesh-mcp --version` equals the built commit | prints the SHA | `flag provided but not defined: -version` |
| `GET /read-counter` | `200` + JSON | `404` |
| `GET /core/sse` | `401` | `401` — a regression check, **not** a discriminator |
| `GET /metrics` | `200` | `200` |
| `GET /sse` | `401` | `401` |
| `sha256(/proc/<pid>/exe)` equals the uploaded bytes | | |

The `/proc/<pid>/exe` hash is the one that catches a failed restart: that leaves
the **old** process running against the **new** file, which `sha256sum` on the
file cannot see.

## Credentials

The workflow uses `secrets.DEPLOY_SSH_KEY` — this repository's **own** key
(`ghdeploy-mesh-mcp@hel01-20260823`), not a copy of evc-mesh's. Revoking one
must not break the other. It is scoped on both hops, and both restrictions were
verified rather than assumed:

* hel01 `ghdeploy`: `restrict,port-forwarding,permitopen="10.10.10.10:22"`, shell
  `/usr/sbin/nologin` — a shell attempt answers *"This account is currently not
  available"*, and a tunnel to any other internal host is refused with
  *"administratively prohibited"*.
* mesh-vm `root`: `restrict` — no pty, no forwarding.

To revoke: delete the secret, then drop the
`ghdeploy-mesh-mcp@hel01-20260823` line from `/home/ghdeploy/.ssh/authorized_keys`
on hel01 and `/root/.ssh/authorized_keys` on mesh-vm. Pre-change copies of both
files are kept beside them as `authorized_keys.bak-20260823-pre-meshmcp`.

## The drill

```bash
scp scripts/mesh-mcp-remote-deploy.sh scripts/mesh-mcp-deploy-drill.sh mesh-vm:/tmp/
ssh mesh-vm 'bash /tmp/mesh-mcp-deploy-drill.sh /tmp/mesh-mcp-remote-deploy.sh'
```

22 assertions: dry-run inertness, refusal of a corrupt upload before the swap,
anchor ordering under six same-second deploys, prune bounds, `rollback` landing
on the immediately previous binary, evc-mesh's anchors left alone, a loud
failure when there is nothing to roll back to, a failing smoke restoring the
previous release by itself, and a drill refusing to run against the production
directory.

The two rollback paths are asserted separately and the distinction is easy to
lose: case 4 covers the **manual** `rollback` command, case 7 covers the
**automatic** restore when a deploy's own smoke fails. Case 7 exists because an
independent review deleted the entire automatic-restore block and the suite
stayed green at 17/17 — the stubbed smoke could not fail, so that path was
unreachable. `DRILL_SMOKE_FAIL=1` makes it reachable; the same deletion is now
caught by exactly one assertion.

CI runs the drill on every build together with a negative control that reverts
the anchor-ordering rule and requires it to go red — a suite that cannot fail
proves nothing. Three genuine ordering defects were found this way before a
single deploy ran; they are described in the comment above `new_anchor_name`.
