// Package webui serves the built React SPA, preferring a directory on disk (the
// --web-root: ui/dist in dev, the slot's web/ folder in prod) and falling back
// to a copy embedded in the binary. Client-side routes fall back to index.html.
//
// The directory comes first because that is what makes a UI change deployable
// without rebuilding the daemon — on this box a rebuild means evicting a 27B and
// paying a cold load, so "reload the browser" is a meaningfully cheaper loop.
// The embedded copy exists because a downloaded binary has no ui/dist at all,
// and a fresh install with no reachable UI cannot be set up through the UI.
package webui

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// Handler serves files under webRoot, falling back to embedded (may be nil) and
// then to a page explaining how to build the UI. Unknown paths resolve to
// index.html for SPA client-side routing; hashed asset files get a long-lived
// cache header while index.html is always revalidated.
func Handler(webRoot string, embedded fs.FS) http.Handler {
	root := pickRoot(webRoot, embedded)
	if root == nil {
		return http.HandlerFunc(notBuilt)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}
		f, err := root.Open(upath)
		if err != nil {
			// Unknown path → SPA shell. The router resolves it client-side.
			w.Header().Set("Cache-Control", "no-cache")
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

// pickRoot chooses the first source that actually has an index.html, or nil.
//
// Presence of index.html is the test, not presence of the directory. --web-root
// defaults to ./ui/dist, which on a machine that merely CONTAINS the repo may
// exist and be empty (gitignored, never built) — treating that as "the UI is
// here" is how every page load became a 404 with no explanation. Likewise the
// embedded copy is a bare .gitkeep unless the build ran pnpm first.
func pickRoot(webRoot string, embedded fs.FS) fs.FS {
	if webRoot != "" {
		if disk := os.DirFS(webRoot); hasIndex(disk) {
			return disk
		}
	}
	if embedded != nil && hasIndex(embedded) {
		return embedded
	}
	return nil
}

func hasIndex(fsys fs.FS) bool {
	f, err := fsys.Open("index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// notBuilt answers when no UI is available anywhere. It says which command
// produces one: a bare 404 here reads as a routing bug and sends people looking
// in the wrong place.
func notBuilt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8">
<title>corrallm — UI not built</title>
<body style="font:14px/1.6 system-ui,sans-serif;max-width:34em;margin:4em auto;padding:0 1em">
<h1 style="font-size:1.3em">The dashboard was not built into this binary</h1>
<p>The API is running normally — this affects only the web UI.</p>
<p>Build it in the repo and restart:</p>
<pre style="background:#f4f4f5;padding:.8em;border-radius:4px">make dist</pre>
<p>Or point the daemon at an existing build:</p>
<pre style="background:#f4f4f5;padding:.8em;border-radius:4px">corrallm serve --web-root /path/to/ui/dist</pre>
</body>`)
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
