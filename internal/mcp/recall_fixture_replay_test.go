package mcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReplayHandlerServesRecordingInOrder(t *testing.T) {
	capture := &recallCapture{}
	srv := httptest.NewServer(newReplayHandler([][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}, capture))
	defer srv.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	for _, other := range []string{"/api/v1/tasks", "/unexpected/api/v1/memories/search"} {
		if code, _ := get(other); code != http.StatusNotFound {
			t.Errorf("%s must be 404, got %d", other, code)
		}
	}
	for i, want := range []string{`{"n":1}`, `{"n":2}`} {
		code, body := get("/api/v1/memories/search")
		if code != http.StatusOK || body != want {
			t.Fatalf("call %d: got %d %q, want 200 %q", i+1, code, body, want)
		}
		if got := string(capture.take()); got != want {
			t.Errorf("call %d: capture holds %q, want %q", i+1, got, want)
		}
	}
	// Negative control: running past the recording must fail, not wrap around.
	if code, _ := get("/api/v1/memories/search"); code != http.StatusInternalServerError {
		t.Errorf("exhausted replay must be 500, got %d", code)
	}
}
