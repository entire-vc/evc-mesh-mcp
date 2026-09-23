package mcp

// BuildSHA is the git commit SHA this binary was built from. Overridden at
// build time via:
//
//	go build -ldflags "-X github.com/entire-vc/evc-mesh-mcp/internal/mcp.BuildSHA=$(git rev-parse HEAD)"
//
// Left at "dev" for local/unpinned builds (go run, go test, go build with no
// ldflags) so a session can tell "not deployed via the pinned build path"
// apart from a real commit SHA — see task #8f441c40 (evc-mesh-mcp had no
// autodeploy; a merged fix silently didn't reach the fleet for 17h and the
// only way to check the installed binary's version was `strings` on the host).
var BuildSHA = "dev"

// Version is the release version (e.g. "1.2.0"). Set at release build time via
//
//	-ldflags "-X github.com/entire-vc/evc-mesh-mcp/internal/mcp.Version=1.2.0"
//
// Empty for untagged builds.
var Version = ""

// ServerVersion is what the server reports in serverInfo.version: the
// release version when there is one, otherwise the build commit.
func ServerVersion() string {
	if Version != "" {
		return Version
	}
	if len(BuildSHA) > 12 {
		return "0.0.0-" + BuildSHA[:12]
	}
	return "0.0.0-" + BuildSHA
}
