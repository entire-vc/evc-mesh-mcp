#!/usr/bin/env bash
#
# Exercises mesh-mcp-remote-deploy.sh against a scratch directory: same script,
# same host, real file operations, systemctl and HTTP stubbed out. It is what
# makes "the rollback anchor works" a re-runnable claim instead of a sentence.
#
# Run it on the deploy target:
#   scp scripts/mesh-mcp-remote-deploy.sh scripts/mesh-mcp-deploy-drill.sh mesh-vm:/tmp/
#   ssh mesh-vm 'bash /tmp/mesh-mcp-deploy-drill.sh /tmp/mesh-mcp-remote-deploy.sh'
#
# Every case below failed at least once during development; three real defects
# in the anchor naming were found this way and are described at new_anchor_name.
set -uo pipefail

SCRIPT="${1:-/opt/evc-mesh/bin/mesh-mcp-remote-deploy.sh}"
WORK="${WORK:-/tmp/mesh-mcp-drill-$$}"
PASS=0
FAIL=0

check() { # check <label> <want> <got>
  if [ "$2" = "$3" ]; then printf '  ok   %-58s %s\n' "$1" "$3"; PASS=$((PASS + 1));
  else printf '  FAIL %-58s want=%s got=%s\n' "$1" "$2" "$3"; FAIL=$((FAIL + 1)); fi
}

run() { DRILL=1 BIN_DIR="$WORK" SERVICE=mesh-mcp-DRILL KEEP_ANCHORS="${KEEP:-3}" EXPECTED_SHA=drillsha bash "$SCRIPT" "$@"; }
stage() { printf '%s\n' "$1" > "$WORK/mesh-mcp.new"; sha256sum "$WORK/mesh-mcp.new" | cut -d' ' -f1 > "$WORK/mesh-mcp.new.sha256"; }
live() { cat "$WORK/mesh-mcp" 2>/dev/null; }
own_anchors() { ls -1 "$WORK"/mesh-mcp.rollback-*-mcprepo 2>/dev/null | sort; }

rm_rf_work() { find "$WORK" -mindepth 1 -delete 2>/dev/null; rmdir "$WORK" 2>/dev/null; }

fresh() {
  rm_rf_work
  mkdir -p "$WORK"
  printf 'v0\n' > "$WORK/mesh-mcp"; chmod +x "$WORK/mesh-mcp"
  # A foreign anchor from evc-mesh's deploy-backend.yml, which writes into the
  # same directory. Nothing here may touch it.
  printf 'FOREIGN\n' > "$WORK/mesh-mcp.rollback-20260101-000000-ci-auto"
}

echo "=== drill: $SCRIPT  (scratch $WORK) ==="

echo "--- 1. dry-run changes nothing and removes the artifact"
fresh; stage v-a
out=$(run dry-run 2>&1); rc=$?
check "dry-run exit code" 0 "$rc"
check "live binary untouched" "v0" "$(live)"
check "uploaded artifact cleaned up" "absent" "$( [ -e "$WORK/mesh-mcp.new" ] && echo present || echo absent )"
check "no anchor written" "0" "$(own_anchors | wc -l | tr -d ' ')"
echo "$out" | grep -q 'would anchor to' || { echo "  FAIL dry-run did not print the plan"; FAIL=$((FAIL + 1)); }

echo "--- 2. corrupt upload is refused BEFORE the swap"
fresh; stage v-a
printf '%064d\n' 0 > "$WORK/mesh-mcp.new.sha256"
run deploy >/dev/null 2>&1; rc=$?
check "deploy exit code on corrupt artifact" 1 "$rc"
check "live binary untouched" "v0" "$(live)"
check "no anchor written by the refused run" "0" "$(own_anchors | wc -l | tr -d ' ')"

echo "--- 3. six same-second deploys: anchors stay ordered, prune keeps KEEP=3"
fresh
for v in a b c d e f; do stage "v-$v"; run deploy >/dev/null 2>&1; done
check "live is the last deploy" "v-f" "$(live)"
check "anchors kept" "3" "$(own_anchors | wc -l | tr -d ' ')"
check "oldest kept anchor" "v-c" "$(cat "$(own_anchors | head -1)")"
check "newest kept anchor" "v-e" "$(cat "$(own_anchors | tail -1)")"
check "no name outside the anchor glob" "0" \
  "$(ls -1 "$WORK" | grep '^mesh-mcp.rollback' | grep -vc -e '-mcprepo$' -e '-ci-auto$')"

echo "--- 4. rollback restores the IMMEDIATELY previous binary"
run rollback >/dev/null 2>&1; rc=$?
check "rollback exit code" 0 "$rc"
check "live after rollback" "v-e" "$(live)"

echo "--- 5. prune never touches evc-mesh's own anchors"
check "ci-auto anchor still present" "1" "$(ls -1 "$WORK"/mesh-mcp.rollback-*-ci-auto 2>/dev/null | wc -l | tr -d ' ')"

echo "--- 6. rollback with no anchor fails loudly instead of silently"
fresh
run rollback >/dev/null 2>&1; rc=$?
check "rollback exit code with no anchor" 1 "$rc"

echo "--- 7. a drill may not run against the production directory"
DRILL=1 BIN_DIR=/opt/evc-mesh/bin bash "$SCRIPT" status >/dev/null 2>&1; rc=$?
check "DRILL against prod BIN_DIR refused" 1 "$rc"

rm_rf_work
echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
