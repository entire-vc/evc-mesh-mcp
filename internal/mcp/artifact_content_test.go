package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// onePixelPNG is a real, complete 1x1 PNG. Genuine bytes rather than a
// hand-written header: the point of these tests is that what lands in storage is
// a file that actually opens.
var onePixelPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0A, 'I', 'D', 'A', 'T',
	0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05,
	0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00,
	0x00, 0x00, 'I', 'E', 'N', 'D', 0xAE, 0x42, 0x60, 0x82,
}

// TestUploadArtifact_Base64_StoresDecodedBytes — the headline defect. The handler
// did []byte(content) unconditionally while the schema advertised
// "text or base64-encoded", so an agent sending a PNG stored the ASCII of
// "iVBORw0KGgo..." and got a success back.
//
// The assertion is on the bytes that left the process, not on the result: before
// the fix this call ALSO succeeded, which is precisely why it survived in prod.
func TestUploadArtifact_Base64_StoresDecodedBytes(t *testing.T) {
	got := captureUpload(t, map[string]any{
		"task_id":       uuid.New().String(),
		"name":          "screenshot.png",
		"content":       base64.StdEncoding.EncodeToString(onePixelPNG),
		"encoding":      "base64",
		"artifact_type": "image",
	})

	if !bytes.Equal(got.fileBytes, onePixelPNG) {
		t.Fatalf("stored bytes are not the decoded PNG: got %d bytes starting %q",
			len(got.fileBytes), firstN(got.fileBytes, 16))
	}
	if string(got.fileBytes) == base64.StdEncoding.EncodeToString(onePixelPNG) {
		t.Fatal("stored the literal base64 string — this is the regression")
	}
	if got.filePart.contentType != "image/png" {
		t.Errorf("declared mime must ride the file part, got %q", got.filePart.contentType)
	}
}

// TestUploadArtifact_TruncatedBase64_Rejected — the load-bearing negative
// control. The same call with a payload cut short must be REFUSED and must not
// reach the server at all. Today it is accepted and answers success.
func TestUploadArtifact_TruncatedBase64_Rejected(t *testing.T) {
	full := base64.StdEncoding.EncodeToString(onePixelPNG)

	got, result := captureUploadResult(t, map[string]any{
		"task_id":       uuid.New().String(),
		"name":          "screenshot.png",
		"content":       full[:len(full)-1], // one char short: the shape of a cut stream
		"encoding":      "base64",
		"artifact_type": "image",
	})

	if !result.IsError {
		t.Fatal("truncated base64 must be refused, not stored")
	}
	if !strings.Contains(resultText(t, result), "base64") {
		t.Errorf("error must name the cause, got: %s", resultText(t, result))
	}
	if got.fields["__reached"] != "no" {
		t.Error("no artifact may be created when the payload is refused")
	}
}

// TestUploadArtifact_AlignedTruncation_CaughtOnlyByChecksum covers the gap the
// length check leaves. A payload cut on a 4-character boundary decodes cleanly
// AND still starts with a valid PNG header, so neither the base64 check nor the
// magic-byte check can see it. Only the checksum can.
//
// The first half is a positive control: it proves the other two guards really do
// let this through, so the second half is attributable to the checksum alone.
func TestUploadArtifact_AlignedTruncation_CaughtOnlyByChecksum(t *testing.T) {
	full := base64.StdEncoding.EncodeToString(onePixelPNG)
	aligned := full[:(len(full)/4)*4-4]

	args := map[string]any{
		"task_id":       uuid.New().String(),
		"name":          "aligned.png",
		"content":       aligned,
		"encoding":      "base64",
		"artifact_type": "image",
	}

	_, control := captureUploadResult(t, args)
	if control.IsError {
		t.Fatalf("premise failed: an aligned truncation should slip past the other guards, got %s",
			resultText(t, control))
	}

	sum := sha256.Sum256(onePixelPNG)
	args["sha256"] = hex.EncodeToString(sum[:])

	got, result := captureUploadResult(t, args)
	if !result.IsError {
		t.Fatal("sha256 mismatch must fail the upload")
	}
	if !strings.Contains(resultText(t, result), "sha256 mismatch") {
		t.Errorf("error must name the checksum, got: %s", resultText(t, result))
	}
	if got.fields["__reached"] != "no" {
		t.Error("nothing may be stored once the checksum fails")
	}
}

// TestUploadArtifact_MimeContradictsContent_Rejected reproduces the exact
// production failure: a base64 string sent WITHOUT encoding, named .png.
func TestUploadArtifact_MimeContradictsContent_Rejected(t *testing.T) {
	got, result := captureUploadResult(t, map[string]any{
		"task_id":       uuid.New().String(),
		"name":          "board.png",
		"content":       base64.StdEncoding.EncodeToString(onePixelPNG),
		"artifact_type": "image",
		// no encoding — exactly what agents were doing
	})

	if !result.IsError {
		t.Fatal("declared image/png with non-PNG bytes must be refused")
	}
	msg := resultText(t, result)
	if !strings.Contains(msg, "does not match declared mime_type") {
		t.Errorf("unexpected error: %s", msg)
	}
	if !strings.Contains(msg, "base64") {
		t.Errorf("error must tell the agent how to fix it, got: %s", msg)
	}
	if got.fields["__reached"] != "no" {
		t.Error("nothing may be stored on a type mismatch")
	}
}

// TestUploadArtifact_LocalPathAsContent_Rejected covers a failure mode the audit
// of live data turned up alongside the base64 one: 7 of 36 broken artifacts held
// a local filesystem path, because the agent reached for a file_path parameter
// that does not exist and its string became the file body — with a 200 back.
func TestUploadArtifact_LocalPathAsContent_Rejected(t *testing.T) {
	_, result := captureUploadResult(t, map[string]any{
		"task_id":       uuid.New().String(),
		"name":          "shot.png",
		"content":       "/Users/someone/screenshots/shot.png",
		"artifact_type": "image",
	})

	if !result.IsError {
		t.Fatal("a path where PNG bytes belong must be refused")
	}
}

// TestUploadArtifact_TextArtifact_Unchanged is the regression guard: the path
// every agent already uses must behave exactly as before. This test passes on
// both the old and the new code, by design.
func TestUploadArtifact_TextArtifact_Unchanged(t *testing.T) {
	const body = "# Report\n\nAll good.\n"

	got := captureUpload(t, map[string]any{
		"task_id":       uuid.New().String(),
		"name":          "report.md",
		"content":       body,
		"artifact_type": "report",
	})

	if string(got.fileBytes) != body {
		t.Errorf("text content must be stored verbatim, got %q", string(got.fileBytes))
	}
}

// TestUploadArtifact_TextThatLooksLikeBase64_StoredAsText is why `encoding` is an
// explicit parameter and not a guess. "deadbeef" is valid base64 and also a
// perfectly ordinary piece of text; a heuristic would silently mangle it.
func TestUploadArtifact_TextThatLooksLikeBase64_StoredAsText(t *testing.T) {
	const body = "deadbeef"

	got := captureUpload(t, map[string]any{
		"task_id": uuid.New().String(),
		"name":    "notes.txt",
		"content": body,
	})

	if string(got.fileBytes) != body {
		t.Errorf("text that merely looks like base64 must not be decoded, got %q", string(got.fileBytes))
	}
}

func TestUploadArtifact_InvalidEncoding_Rejected(t *testing.T) {
	got, result := captureUploadResult(t, map[string]any{
		"task_id":  uuid.New().String(),
		"name":     "x.txt",
		"content":  "hello",
		"encoding": "hex",
	})

	if !result.IsError || !strings.Contains(resultText(t, result), "invalid encoding") {
		t.Fatalf("unknown encoding must be refused, got: %s", resultText(t, result))
	}
	if got.fields["__reached"] != "no" {
		t.Error("nothing may be stored on an unknown encoding")
	}
}

// --- helper unit tests ------------------------------------------------------

func TestDecodeArtifactContent(t *testing.T) {
	png64 := base64.StdEncoding.EncodeToString(onePixelPNG)

	tests := []struct {
		name     string
		content  string
		encoding string
		want     []byte
		wantErr  string
	}{
		{name: "default is text", content: "hello", want: []byte("hello")},
		{name: "explicit text", content: "hello", encoding: "text", want: []byte("hello")},
		{name: "base64 decodes", content: png64, encoding: "base64", want: onePixelPNG},
		{name: "case insensitive", content: png64, encoding: "BASE64", want: onePixelPNG},
		{name: "wrapped base64", content: png64[:8] + "\n" + png64[8:], encoding: "base64", want: onePixelPNG},
		{name: "truncated", content: png64[:len(png64)-1], encoding: "base64", wantErr: "not a multiple of 4"},
		{name: "garbage", content: "not!valid!base64", encoding: "base64", wantErr: "not valid base64"},
		{name: "unknown encoding", content: "x", encoding: "hex", wantErr: "invalid encoding"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeArtifactContent(tt.content, tt.encoding)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestValidateArtifactMagic(t *testing.T) {
	tests := []struct {
		name    string
		mime    string
		data    []byte
		wantErr bool
	}{
		{name: "png ok", mime: "image/png", data: onePixelPNG},
		{name: "png with params", mime: "image/png; charset=binary", data: onePixelPNG},
		{name: "png against base64 text", mime: "image/png", data: []byte("iVBORw0KGgoAAAA"), wantErr: true},
		{name: "png against a path", mime: "image/png", data: []byte("/Users/x/shot.png"), wantErr: true},
		{name: "jpeg ok", mime: "image/jpeg", data: []byte{0xFF, 0xD8, 0xFF, 0xE0}},
		{name: "jpeg against png", mime: "image/jpeg", data: onePixelPNG, wantErr: true},
		{name: "pdf ok", mime: "application/pdf", data: []byte("%PDF-1.7\n")},
		{name: "zip ok", mime: "application/zip", data: []byte{'P', 'K', 0x03, 0x04}},
		{name: "gif ok", mime: "image/gif", data: []byte("GIF89a...")},
		// One-directional by design: unknown types are not policed.
		{name: "unknown mime passes", mime: "text/markdown", data: []byte("# hi")},
		{name: "empty passes", mime: "image/png", data: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateArtifactMagic(tt.mime, tt.data)
			if tt.wantErr && err == nil {
				t.Fatal("expected a rejection")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}

func TestVerifyArtifactChecksum(t *testing.T) {
	data := []byte("payload")
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])

	if err := verifyArtifactChecksum("", data); err != nil {
		t.Errorf("empty checksum must be a no-op: %v", err)
	}
	if err := verifyArtifactChecksum(hexSum, data); err != nil {
		t.Errorf("matching checksum must pass: %v", err)
	}
	if err := verifyArtifactChecksum(hexSum, []byte("payloa")); err == nil {
		t.Error("mismatched checksum must fail")
	}
}

func firstN(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}
