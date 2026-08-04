package agentdist

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/iodesystems/corrallm/internal/agent"
)

// BuildID returns the content hash of the agent binary this primary would hand
// a goos/goarch machine, or "" when it serves none.
//
// This is what the heartbeat advertises, and it is deliberately the identity of
// the FILE rather than the version stamped into it: `make agents` without a tag
// writes "dev", and two different dev builds compare equal. See agent.HashFile
// for why that ends self-update during exactly the work it exists to support.
//
// Cached by (size, mtime): the binary is ~37 MB and every attached agent asks on
// every heartbeat, so hashing per request would burn real I/O on a file that
// changes only when someone runs a build.
func (h *Handler) BuildID(goos, goarch string) string {
	if h == nil || goos == "" || goarch == "" {
		return ""
	}
	path := filepath.Join(h.Dir, fmt.Sprintf("corrallm-%s-%s", goos, goarch))
	id, err := cachedFileID(path)
	if err != nil {
		return ""
	}
	return id
}

type fileID struct {
	size  int64
	mtime int64
	id    string
}

var (
	idMu    sync.Mutex
	idCache = map[string]fileID{}
)

// cachedFileID hashes path, reusing the last result while size and mtime are
// unchanged.
//
// mtime+size is a weaker identity than the hash, which is why it is only the
// CACHE KEY and never the answer: a rebuild moves mtime and the hash is
// recomputed. Two different binaries sharing a size and an mtime to the
// nanosecond is not a case worth engineering against.
func cachedFileID(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a directory", path)
	}

	idMu.Lock()
	hit, ok := idCache[path]
	idMu.Unlock()
	if ok && hit.size == st.Size() && hit.mtime == st.ModTime().UnixNano() {
		return hit.id, nil
	}

	id, err := agent.HashFile(path)
	if err != nil {
		return "", err
	}
	idMu.Lock()
	idCache[path] = fileID{size: st.Size(), mtime: st.ModTime().UnixNano(), id: id}
	idMu.Unlock()
	return id, nil
}
