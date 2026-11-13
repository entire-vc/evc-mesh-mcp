package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// maxLocalArtifactBytes caps a file_path upload. It matches the API's default
// ARTIFACT_MAX_BYTES (25 MB), so a file the API would refuse anyway is refused
// here before it is read into memory and sent.
const maxLocalArtifactBytes int64 = 25 << 20

// errFilePathNotLocal is returned when file_path is used on a transport whose
// process is not the caller's: over SSE/HTTP the server runs on another host
// and "a path" names a file on the server, not on the agent's machine.
var errFilePathNotLocal = errors.New(
	"file_path is only available over stdio (a mesh-mcp process running on your machine); " +
		"this server is remote and cannot read your disk. Send the bytes as content with encoding=\"base64\" " +
		"and sha256, or POST the file as multipart to /api/v1/tasks/{task_id}/artifacts")

// readArtifactFile reads a file named by an upload_artifact file_path.
//
// It exists because inline base64 crosses the model's context on its way to the
// tool call, and models drop chunks from payloads of a few KB (a 6685-byte jpg
// arrived as 6481 bytes; only the sha256 guard caught it). Reading the file in
// this process moves the bytes disk → API without the model ever holding them.
//
// Refused: relative paths (the server's working directory is not the agent's),
// symlinks and anything that is not a regular file, and files over the limit.
// The final component is checked with Lstat and the opened file is compared to
// it, so a path swapped for a symlink between the check and the open is refused
// too. Symlinks in parent directories (/tmp on macOS) are allowed.
func readArtifactFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("file_path must be absolute, got %q", path)
	}
	path = filepath.Clean(path)

	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("file_path: %w", err)
	}
	if err := checkRegularFile(path, before); err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("file_path: %w", err)
	}
	defer f.Close()

	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("file_path: %w", err)
	}
	if !os.SameFile(before, opened) {
		return nil, fmt.Errorf("file_path %q changed while being opened; refusing", path)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxLocalArtifactBytes+1))
	if err != nil {
		return nil, fmt.Errorf("file_path: read: %w", err)
	}
	if int64(len(data)) > maxLocalArtifactBytes {
		return nil, fmt.Errorf("file_path %q is larger than the %d-byte limit", path, maxLocalArtifactBytes)
	}
	return data, nil
}

func checkRegularFile(path string, fi os.FileInfo) error {
	mode := fi.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		return fmt.Errorf("file_path %q is a symlink; pass the path of the file itself", path)
	case mode.IsDir():
		return fmt.Errorf("file_path %q is a directory, not a file", path)
	case !mode.IsRegular():
		return fmt.Errorf("file_path %q is not a regular file (mode %s)", path, mode.Type())
	case fi.Size() > maxLocalArtifactBytes:
		return fmt.Errorf("file_path %q is %d bytes, over the %d-byte limit", path, fi.Size(), maxLocalArtifactBytes)
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// withUploadedChecksum adds the sha256 and size of the bytes this process sent,
// so the caller can compare them with `shasum -a 256` of its file without a
// download. sha256_matches_stored compares them with the checksum the API
// computed on receipt, when the API returned one.
func withUploadedChecksum(out map[string]any, data []byte) map[string]any {
	if out == nil {
		out = map[string]any{}
	}
	sum := sha256Hex(data)
	out["sha256"] = sum
	out["uploaded_bytes"] = len(data)
	if stored, ok := out["checksum_sha256"].(string); ok && stored != "" {
		out["sha256_matches_stored"] = stored == sum
	}
	return out
}
