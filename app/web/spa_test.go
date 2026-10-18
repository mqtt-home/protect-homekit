package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSPAHandler(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "index-abc.css"), []byte("body{}"), 0o644)
	h := spaHandler(dir+"/", http.FileServer(http.Dir(dir)))

	cases := []struct {
		path, cache string
		code        int
	}{
		{"/", "no-cache", 200},
		{"/rules/x", "no-cache", 200},
		{"/assets/index-abc.css", "public, max-age=31536000, immutable", 200},
		{"/assets/index-old.css", "", 404},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != c.code || rec.Header().Get("Cache-Control") != c.cache {
			t.Errorf("%s: got %d %q, want %d %q", c.path, rec.Code, rec.Header().Get("Cache-Control"), c.code, c.cache)
		}
	}
}
