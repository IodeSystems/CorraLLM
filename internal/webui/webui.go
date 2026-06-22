// Package webui serves the built React SPA from a directory on disk (the
// --web-root: ui/dist in dev, the slot's web/ folder in prod). Same approach as
// redline2 — the UI ships as a folder, not go:embed bytes, so it can be swapped
// without rebuilding the binary. Client-side routes fall back to index.html.
package webui

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// Handler serves files under webRoot, falling back to index.html for unknown
// paths (SPA client-side routing). Hashed asset files get a long-lived cache
// header; index.html is always revalidated.
func Handler(webRoot string) http.Handler {
	root := os.DirFS(webRoot)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}
		f, err := root.Open(upath)
		if err != nil {
			// Unknown path → SPA shell. The router resolves it client-side.
			serveFile(w, r, root, "index.html")
			return
		}
		_ = f.Close()
		// Vite emits immutable, content-hashed assets under /assets/.
		if strings.HasPrefix(upath, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		serveFile(w, r, root, upath)
	})
}

func serveFile(w http.ResponseWriter, r *http.Request, root fs.FS, name string) {
	f, err := root.Open(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "not seekable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, name, st.ModTime(), rs)
}
