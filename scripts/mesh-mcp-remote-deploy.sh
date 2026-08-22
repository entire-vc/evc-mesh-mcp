#!/usr/bin/env bash
#
# Remote half of the mesh-vm deploy for this repository's SSE binary.
#
# This file lives in the repo and is re-uploaded by the workflow on EVERY run,
# so the copy that executes on prod is the copy that was reviewed here. That is
# deliberate: the same logic used to live inline in a workflow heredoc, where it
# could not be read, tested, or exercised outside a real production deploy.
#
#   deploy    swap in bin/mesh-mcp.new, restart the service, smoke it,
#             and roll back automatically if the smoke fails
#   dry-run   verify the uploaded artifact and print the exact plan; touches
#             neither the live binary nor the service
#   rollback  restore the newest anchor this script created and restart
#   status    report what is currently live
#
# Env overrides (defaults are the production values):
#   BIN_DIR       /opt/evc-mesh/bin
#   SERVICE       mesh-mcp
#   HEALTH_URL    http://127.0.0.1:8081
#   KEEP_ANCHORS  10
#   EXPECTED_SHA  git SHA the binary was built from; asserted via `--version`
#   DRILL         1 = file operations real, systemctl/HTTP replaced by no-ops.
#                 Refused when BIN_DIR is the production directory, so a drill
#                 can never silently "pass" against prod.
set -uo pipefail

BIN_DIR="${BIN_DIR:-/opt/evc-mesh/bin}"
SERVICE="${SERVICE:-mesh-mcp}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8081}"
KEEP_ANCHORS="${KEEP_ANCHORS:-10}"
EXPECTED_SHA="${EXPECTED_SHA:-}"
DRILL="${DRILL:-0}"

PROD_BIN_DIR=/opt/evc-mesh/bin
LIVE="$BIN_DIR/mesh-mcp"
INCOMING="$BIN_DIR/mesh-mcp.new"
INCOMING_SHA="$BIN_DIR/mesh-mcp.new.sha256"

# Anchors written by THIS pipeline carry the -mcprepo suffix. evc-mesh's
# deploy-backend.yml writes -ci-auto anchors into the same directory; pruning
# is scoped to our own suffix so this script never deletes another pipeline's
# rollback material.
ANCHOR_GLOB="mesh-mcp.rollback-*-mcprepo"

die() { echo "ERROR: $*" >&2; exit 1; }
note() { echo "  $*"; }

if [ "$DRILL" = "1" ] && [ "$BIN_DIR" = "$PROD_BIN_DIR" ]; then
  die "DRILL=1 refused against the production BIN_DIR ($PROD_BIN_DIR). A drill must use a scratch directory."
fi
if [ "$DRILL" = "1" ]; then
  echo "################ DRILL MODE — systemctl and HTTP probes are no-ops ################"
fi

sha_of() { sha256sum "$1" | cut -d' ' -f1; }

restart_service() {
  if [ "$DRILL" = "1" ]; then note "[drill] would: systemctl restart $SERVICE"; return 0; fi
  systemctl restart "$SERVICE"
}

# Hash the binary the running process actually executes, not the file on disk.
# A failed restart leaves the OLD process running against the NEW file, and
# `sha256sum $LIVE` cannot see that.
running_exe_sha() {
  local pid
  pid=$(systemctl show "$SERVICE" -p MainPID --value 2>/dev/null)
  [ -n "$pid" ] && [ "$pid" != "0" ] || { echo ""; return 0; }
  sha256sum "/proc/$pid/exe" 2>/dev/null | cut -d' ' -f1
}

http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$HEALTH_URL$1" 2>/dev/null || echo "000"; }

# Every assertion below is content, not "the service answered". Each one can
# tell the NEW binary from the one deployed out of evc-mesh/cmd/mcp:
#   --version       new: prints the build SHA;      old: "flag provided but not defined"
#   /read-counter   new: 200 JSON snapshot;         old: 404
#   /core/sse       both: 401 — a regression check, NOT a discriminator
smoke() {
  local want_sha="$1" i rc

  if [ "$DRILL" = "1" ]; then note "[drill] would: smoke $SERVICE at $HEALTH_URL"; return 0; fi

  local live_sha
  live_sha=""
  for i in $(seq 1 15); do
    live_sha=$(running_exe_sha)
    [ "$live_sha" = "$want_sha" ] && break
    echo "  waiting for $SERVICE to come up on the new binary (attempt $i/15): ${live_sha:-no pid}"
    sleep 2
  done
  [ "$live_sha" = "$want_sha" ] || { echo "SMOKE FAIL: running process is $live_sha, expected $want_sha" >&2; return 1; }
  note "running process executes the uploaded bytes ($want_sha)"

  if [ -n "$EXPECTED_SHA" ]; then
    local reported
    reported=$("$LIVE" --version 2>&1 | head -1)
    [ "$reported" = "$EXPECTED_SHA" ] || {
      echo "SMOKE FAIL: --version reported '$reported', expected '$EXPECTED_SHA'" >&2; return 1; }
    note "--version reports the built commit ($reported)"
  fi

  rc=$(http_code /metrics);       [ "$rc" = "200" ] || { echo "SMOKE FAIL: /metrics -> $rc (want 200)" >&2; return 1; }
  rc=$(http_code /read-counter);  [ "$rc" = "200" ] || { echo "SMOKE FAIL: /read-counter -> $rc (want 200)" >&2; return 1; }
  rc=$(http_code /sse);           [ "$rc" = "401" ] || { echo "SMOKE FAIL: /sse -> $rc (want 401, unauthenticated)" >&2; return 1; }
  rc=$(http_code /core/sse);      [ "$rc" = "401" ] || { echo "SMOKE FAIL: /core/sse -> $rc (want 401 — 404 means the core profile is not routed)" >&2; return 1; }
  note "endpoints: /metrics 200 · /read-counter 200 · /sse 401 · /core/sse 401"
  return 0
}

# Anchor names are second-granular, so two deploys in the same second would
# make `cp` overwrite the first anchor silently — losing exactly the artifact
# the anchor exists to preserve. The burst guard makes that unlikely, not
# impossible (force=true skips it), so every name carries a 2-digit sequence.
#
# Two rules here, both bought with a failed drill run:
#
#  1. The sequence sits BEFORE the -mcprepo suffix and is zero-padded. A first
#     attempt appended ".2" AFTER the suffix; those names fell outside
#     ANCHOR_GLOB, invisible to both prune (leak forever) and newest_anchor
#     (rollback would silently restore an older anchor than the one just made).
#
#  2. The sequence is one past the HIGHEST existing one for that second, not
#     the lowest free slot. Taking the lowest free slot looks equivalent and is
#     not: prune frees the low numbers first, so the next anchor reclaimed -00,
#     sorted to the front as the oldest, and was pruned by its own deploy —
#     leaving `rollback` pointing one release too far back. Measured, not
#     theorised: the 5-in-one-second drill restored v-c where v-d was live.
#
#  3. The sequence is parsed off a prefix this function BUILDS, not off a
#     hand-written glob. The hand-written one said "-rollback-" where the name
#     says ".rollback-", so it matched nothing, every same-second anchor name
#     came back empty, and `cp` failed onto the directory itself.
new_anchor_name() {
  local stamp prefix n base seq
  stamp=$(date -u +%Y%m%d-%H%M%S)
  prefix="mesh-mcp.rollback-$stamp-"
  n=0
  for f in "$BIN_DIR/$prefix"[0-9][0-9]-mcprepo; do
    [ -e "$f" ] || continue
    base=${f##*/}
    seq=${base#"$prefix"}
    seq=${seq%-mcprepo}
    case "$seq" in
      [0-9][0-9]) ;;
      *) continue ;;          # not one of ours in the expected shape — ignore
    esac
    seq=$((10#$seq))
    [ "$seq" -ge "$n" ] && n=$((seq + 1))
  done
  # Beyond 99 the padding would widen and break the lexicographic ordering
  # every other function here depends on. Unreachable in practice; loud rather
  # than silently wrong if it ever is not.
  [ "$n" -le 99 ] || die "100 anchors already exist for second $stamp — refusing to write an unsortable name"
  printf '%s%02d-mcprepo' "$prefix" "$n"
}

newest_anchor() {
  # shellcheck disable=SC2012  # names are pipeline-generated and timestamp-sorted
  ls -1 "$BIN_DIR"/$ANCHOR_GLOB 2>/dev/null | sort | tail -1
}

prune_anchors() {
  local count
  count=$(ls -1 "$BIN_DIR"/$ANCHOR_GLOB 2>/dev/null | wc -l | tr -d ' ')
  [ "$count" -gt "$KEEP_ANCHORS" ] || { note "anchors: $count (keeping $KEEP_ANCHORS, nothing to prune)"; return 0; }
  ls -1 "$BIN_DIR"/$ANCHOR_GLOB | sort | head -n -"$KEEP_ANCHORS" | while read -r old; do
    note "pruning old anchor $(basename "$old")"
    unlink "$old"
  done
}

verify_incoming() {
  [ -f "$INCOMING" ]     || die "no uploaded binary at $INCOMING"
  [ -f "$INCOMING_SHA" ] || die "no checksum sidecar at $INCOMING_SHA"
  local want got
  want=$(cat "$INCOMING_SHA")
  got=$(sha_of "$INCOMING")
  # Checked BEFORE the swap, unlike the reference in evc-mesh's
  # deploy-backend.yml which asserts only afterwards — by which point a
  # truncated upload has already replaced the live binary.
  [ "$want" = "$got" ] || die "uploaded artifact is corrupt: sidecar says $want, file hashes to $got"
  echo "$got"
}

cmd_dry_run() {
  local sha anchor
  sha=$(verify_incoming) || exit 1
  anchor=$(new_anchor_name)
  echo "DRY RUN — nothing on this host was modified."
  note "uploaded artifact verified: $sha"
  [ -n "$EXPECTED_SHA" ] && note "built from commit: $EXPECTED_SHA"
  note "live binary now:  $( [ -f "$LIVE" ] && sha_of "$LIVE" || echo '(absent)')"
  note "would anchor to:  $BIN_DIR/$anchor"
  note "would swap:       $INCOMING -> $LIVE"
  note "would restart:    systemctl restart $SERVICE"
  note "would smoke:      --version == $EXPECTED_SHA, /metrics 200, /read-counter 200, /sse 401, /core/sse 401"
  note "rollback if smoke fails (automatic, and runnable by hand):"
  note "    $0 rollback"
  note "own anchors present: $(ls -1 "$BIN_DIR"/$ANCHOR_GLOB 2>/dev/null | wc -l | tr -d ' ') (keep $KEEP_ANCHORS)"
  unlink "$INCOMING"
  unlink "$INCOMING_SHA"
  note "uploaded artifact removed — the host is byte-for-byte as it was."
}

cmd_deploy() {
  local sha anchor anchor_path post
  sha=$(verify_incoming) || exit 1

  anchor=$(new_anchor_name)
  anchor_path="$BIN_DIR/$anchor"
  if [ -f "$LIVE" ]; then
    cp -p "$LIVE" "$anchor_path" || die "could not write the rollback anchor — refusing to swap"
    note "rollback anchor: $anchor ($(sha_of "$anchor_path"))"
  else
    note "no live binary to anchor (first install)"
  fi

  mv "$INCOMING" "$LIVE"
  unlink "$INCOMING_SHA"
  post=$(sha_of "$LIVE")
  [ "$post" = "$sha" ] || die "post-swap assertion failed: expected $sha got $post"
  note "swapped in $post"

  restart_service

  if smoke "$sha"; then
    prune_anchors
    echo "DEPLOY OK — $SERVICE is live on $sha"
    return 0
  fi

  echo "SMOKE FAILED — rolling back automatically" >&2
  if [ -f "$anchor_path" ]; then
    cp -p "$anchor_path" "$LIVE"
    restart_service
    sleep 3
    echo "rolled back to $anchor; live process now $(running_exe_sha)" >&2
  else
    echo "no anchor to roll back to (first install) — $SERVICE left on the new binary" >&2
  fi
  exit 1
}

cmd_rollback() {
  local anchor
  anchor="${ANCHOR:-$(newest_anchor)}"
  [ -n "$anchor" ] && [ -f "$anchor" ] || die "no anchor matching $ANCHOR_GLOB in $BIN_DIR"
  note "restoring $(basename "$anchor")"
  cp -p "$anchor" "$LIVE"
  restart_service
  sleep 3
  local want got
  want=$(sha_of "$LIVE")
  got=$(running_exe_sha)
  if [ "$DRILL" = "1" ]; then
    echo "ROLLBACK OK (drill) — $LIVE now $want"
    return 0
  fi
  [ "$want" = "$got" ] || die "rollback restarted but the process runs $got, not $want"
  echo "ROLLBACK OK — $SERVICE is live on $want (from $(basename "$anchor"))"
}

cmd_status() {
  echo "service:  $SERVICE ($( [ "$DRILL" = "1" ] && echo drill || systemctl is-active "$SERVICE" 2>/dev/null))"
  echo "live bin: $( [ -f "$LIVE" ] && sha_of "$LIVE" || echo absent)"
  echo "running:  $( [ "$DRILL" = "1" ] && echo '(drill)' || running_exe_sha)"
  echo "version:  $( [ -x "$LIVE" ] && ("$LIVE" --version 2>&1 | head -1) || echo n/a)"
  echo "anchors:  $(ls -1 "$BIN_DIR"/$ANCHOR_GLOB 2>/dev/null | wc -l | tr -d ' ') own / $(ls -1 "$BIN_DIR"/mesh-mcp.rollback-*-ci-auto 2>/dev/null | wc -l | tr -d ' ') from evc-mesh"
  if [ "$DRILL" != "1" ]; then
    for p in /metrics /read-counter /sse /core/sse; do echo "  $p -> $(http_code $p)"; done
  fi
}

case "${1:-}" in
  deploy)   cmd_deploy ;;
  dry-run)  cmd_dry_run ;;
  rollback) cmd_rollback ;;
  status)   cmd_status ;;
  *) die "usage: $0 <deploy|dry-run|rollback|status>" ;;
esac
