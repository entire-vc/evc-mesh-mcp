package mcp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// fileUpload runs handleUploadArtifact against a fake API. localFiles mirrors
// the transport: true is what main.go sets for stdio, false (the zero value) is
// what SSE/HTTP servers get.
type fileUpload struct {
	reached  bool
	filename string
	body     []byte
	result   *mcpsdk.CallToolResult
}

func runFileUpload(t *testing.T, localFiles bool, args map[string]any) fileUpload {
	t.Helper()
	var out fileUpload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/artifacts") {
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		out.reached = true
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			if p.FileName() != "" {
				out.filename = p.FileName()
				out.body, _ = io.ReadAll(p)
			}
		}
		// The real API answers with the checksum it computed on receipt.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":              uuid.New().String(),
			"checksum_sha256": sha256Hex(out.body),
			"size_bytes":      len(out.body),
		})
	}))
	t.Cleanup(srv.Close)

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker(), localFiles: localFiles}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	ctx := withTestSession(context.Background(), server, uuid.New())
	res, err := server.handleUploadArtifact(ctx, req)
	if err != nil {
		t.Fatalf("handleUploadArtifact: %v", err)
	}
	out.result = res
	return out
}

func uploadText(res *mcpsdk.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(mcpsdk.TextContent); ok {
		return tc.Text
	}
	return ""
}

// writeJPEG writes a jpeg-signed file of n random bytes — the size of the file
// that lost 204 bytes when sent inline.
func writeJPEG(t *testing.T, dir string, n int) (string, []byte) {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	copy(data, []byte{0xFF, 0xD8, 0xFF, 0xE0})
	path := filepath.Join(dir, "shot.jpg")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

// Criterion 1: over stdio the bytes on disk are the bytes the API receives,
// name defaults to the basename, and the response carries their sha256.
func TestUploadArtifact_FilePath_StdioUploadsTheExactBytes(t *testing.T) {
	path, data := writeJPEG(t, t.TempDir(), 6685)

	up := runFileUpload(t, true, map[string]any{"task_id": uuid.New().String(), "file_path": path})
	if up.result.IsError {
		t.Fatalf("upload refused: %s", uploadText(up.result))
	}
	if !up.reached {
		t.Fatal("nothing reached the API")
	}
	if len(up.body) != len(data) || sha256Hex(up.body) != sha256Hex(data) {
		t.Fatalf("API received %d bytes sha256 %s, file has %d bytes sha256 %s",
			len(up.body), sha256Hex(up.body), len(data), sha256Hex(data))
	}
	if up.filename != "shot.jpg" {
		t.Errorf("name should default to the basename, got %q", up.filename)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(uploadText(up.result)), &got); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if got["sha256"] != sha256Hex(data) {
		t.Errorf("response sha256 = %v, want %s", got["sha256"], sha256Hex(data))
	}
	if got["sha256_matches_stored"] != true {
		t.Errorf("sha256_matches_stored = %v, want true", got["sha256_matches_stored"])
	}
}

func TestUploadArtifact_FilePath_ExplicitNameWinsAndChecksumIsEnforced(t *testing.T) {
	path, data := writeJPEG(t, t.TempDir(), 2048)

	up := runFileUpload(t, true, map[string]any{
		"task_id": uuid.New().String(), "file_path": path, "name": "renamed.jpg", "sha256": sha256Hex(data),
	})
	if up.result.IsError || up.filename != "renamed.jpg" {
		t.Fatalf("want upload as renamed.jpg, got error=%v filename=%q: %s", up.result.IsError, up.filename, uploadText(up.result))
	}

	bad := runFileUpload(t, true, map[string]any{
		"task_id": uuid.New().String(), "file_path": path, "sha256": strings.Repeat("0", 64),
	})
	if !bad.result.IsError || bad.reached {
		t.Fatalf("a wrong sha256 must refuse before any request: error=%v reached=%v", bad.result.IsError, bad.reached)
	}
}

// Criterion 2: the same call on a non-stdio server is refused and creates nothing.
func TestUploadArtifact_FilePath_RefusedOutsideStdio(t *testing.T) {
	path, _ := writeJPEG(t, t.TempDir(), 6685)

	up := runFileUpload(t, false, map[string]any{"task_id": uuid.New().String(), "file_path": path})
	if !up.result.IsError {
		t.Fatalf("file_path must be refused when localFiles is off: %s", uploadText(up.result))
	}
	if up.reached {
		t.Fatal("a refused file_path upload reached the API")
	}
	if !strings.Contains(uploadText(up.result), "file_path is only available over stdio") {
		t.Errorf("refusal must say why: %s", uploadText(up.result))
	}
}

// The zero ServerConfig (what SSE/HTTP servers are built with) must fail closed.
func TestUploadArtifact_FilePath_ZeroConfigFailsClosed(t *testing.T) {
	if NewServer(ServerConfig{}).localFiles {
		t.Fatal("ServerConfig{} must not enable file_path")
	}
	if !NewServer(ServerConfig{LocalFiles: true}).localFiles {
		t.Fatal("LocalFiles: true must enable file_path")
	}
}

// Criterion 3: content + file_path together, a directory, a symlink, a relative
// path and a missing file are refused, and none of them reaches the API.
func TestUploadArtifact_FilePath_Refusals(t *testing.T) {
	dir := t.TempDir()
	path, _ := writeJPEG(t, dir, 512)
	link := filepath.Join(dir, "link.jpg")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"content and file_path", map[string]any{"file_path": path, "content": "x", "name": "a.txt"}, "mutually exclusive"},
		{"directory", map[string]any{"file_path": dir}, "is a directory"},
		{"symlink", map[string]any{"file_path": link}, "is a symlink"},
		{"relative", map[string]any{"file_path": "shot.jpg"}, "must be absolute"},
		{"missing", map[string]any{"file_path": filepath.Join(dir, "nope.jpg")}, "no such file"},
		{"neither", map[string]any{"name": "a.txt"}, "content or file_path is required"},
		{"content without name", map[string]any{"content": "x"}, "name is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.args["task_id"] = uuid.New().String()
			up := runFileUpload(t, true, tc.args)
			if !up.result.IsError {
				t.Fatalf("want refusal, got success: %s", uploadText(up.result))
			}
			if up.reached {
				t.Fatal("a refused upload reached the API")
			}
			if !strings.Contains(uploadText(up.result), tc.want) {
				t.Errorf("refusal %q does not contain %q", uploadText(up.result), tc.want)
			}
		})
	}
}

func TestReadArtifactFile_RefusesOverLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxLocalArtifactBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := readArtifactFile(path); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("want size-limit refusal, got %v", err)
	}
}

// Inline content keeps working unchanged and also reports its sha256.
func TestUploadArtifact_InlineContentStillWorks(t *testing.T) {
	up := runFileUpload(t, false, map[string]any{"task_id": uuid.New().String(), "name": "a.txt", "content": "hello"})
	if up.result.IsError || string(up.body) != "hello" {
		t.Fatalf("inline upload broke: error=%v body=%q %s", up.result.IsError, up.body, uploadText(up.result))
	}
	if !strings.Contains(uploadText(up.result), sha256Hex([]byte("hello"))) {
		t.Errorf("inline response must carry sha256: %s", uploadText(up.result))
	}
}
