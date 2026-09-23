#!/usr/bin/env bash
# Publish a released MCPB bundle to Smithery with the full tool list.
#
#   SMITHERY_API_KEY=... scripts/smithery-publish.sh <version> [namespace/name]
#
# Why not `smithery mcp publish`: for stdio bundles the CLI copies the
# manifest's `tools` into the listing, and the MCPB manifest format allows only
# name/description there while the Smithery API requires inputSchema, so the
# listing ends up with no tools. This script sends the same request the CLI
# sends (PUT /servers/<name>/releases, multipart `payload` + `bundle`), with the
# tool list taken from the server's own tools/list. The server lists its tools
# without credentials, so no Mesh key is needed.
set -euo pipefail
VERSION="${1:?usage: $0 <version> [namespace/name]}"
NAME="${2:-entirevc/evc-mesh-mcp}"
: "${SMITHERY_API_KEY:?set SMITHERY_API_KEY}"

WORK="$(mktemp -d)"
trap 'find "$WORK" -mindepth 1 -delete; rmdir "$WORK"' EXIT

BUNDLE="$WORK/evc-mesh-mcp-$VERSION.mcpb"
curl -fsSL -o "$BUNDLE" \
  "https://github.com/entire-vc/evc-mesh-mcp/releases/download/v$VERSION/evc-mesh-mcp-$VERSION.mcpb"
unzip -q "$BUNDLE" -d "$WORK/b"

case "$(uname -s)" in
  Darwin) BIN="$WORK/b/server/evc-mesh-mcp-darwin-universal" ;;
  Linux)  BIN="$WORK/b/server/evc-mesh-mcp-linux-amd64" ;;
  *) echo "unsupported OS" >&2; exit 1 ;;
esac
chmod +x "$BIN"

printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smithery-publish","version":"1"}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  | (cat; sleep 1) \
  | env -u MESH_AGENT_KEY -u MESH_API_URL MESH_MCP_PROFILE=full "$BIN" 2>/dev/null > "$WORK/tools.jsonl"

python3 - "$WORK" <<'PY'
import json, sys
w = sys.argv[1]
m = json.load(open(f"{w}/b/manifest.json"))
tools = [json.loads(l) for l in open(f"{w}/tools.jsonl") if l.strip()][-1]["result"]["tools"]
if not tools:
    sys.exit("tools/list returned nothing — refusing to publish an empty listing")
props, req = {}, []
for k, v in m.get("user_config", {}).items():
    p = {"type": v["type"]}
    for f in ("title", "description", "default"):
        if f in v:
            p[f] = v[f]
    props[k] = p
    if v.get("required"):
        req.append(k)
payload = {
    "type": "stdio",
    "runtime": "binary",
    "serverCard": {"serverInfo": {"name": m["name"], "version": m["version"]}, "tools": tools},
    "configSchema": {"type": "object", "properties": props, "required": req},
}
json.dump(payload, open(f"{w}/payload.json", "w"))
print(f"publishing {m['name']} {m['version']} with {len(tools)} tools")
PY

curl -fsS -X PUT \
  -H "Authorization: Bearer $SMITHERY_API_KEY" \
  -F "payload=<$WORK/payload.json;type=application/json" \
  -F "bundle=@$BUNDLE;type=application/octet-stream" \
  "https://api.smithery.ai/servers/$NAME/releases"
echo
