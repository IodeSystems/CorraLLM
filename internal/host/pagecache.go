package host

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Every spawn reads its weights from DISK, never from the page cache.
//
// llama.cpp mmaps the model file. System RAM here has no ECC, so a bit that
// flipped in a cached page of the file would be copied straight back into the
// next load — the reload meant to clear a flip (proc's non-ECC refresh) would
// reload the flip. So before each spawn the model files the command names are
// dropped from the page cache (posix_fadvise DONTNEED; Linux only, a no-op
// elsewhere).
//
// It runs after the previous process of the model has exited — the manager
// waits for that before it starts a spawn — because pages a live process still
// maps are not dropped. For the same reason a file that ANOTHER running
// backend maps keeps its pages; DONTNEED then does nothing for those pages,
// which is harmless, so there is no logic to avoid it.

// modelExts are the extensions treated as model files. Only these: a command
// also names the llama-server binary, chat templates and grammar files, and
// dropping those from the cache costs a slower start for no gain.
var modelExts = []string{".gguf", ".bin", ".safetensors"}

// reShard matches one shard of a split gguf: <stem>-00001-of-00003.gguf.
var reShard = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// EvictPageCache drops the page cache of every model file cmd names, and logs
// what it dropped. Best effort: a file that cannot be opened is logged and
// skipped; nothing here can fail a spawn.
func EvictPageCache(cmd string) { evictPageCache(cmd, "before spawn") }

func evictPageCache(cmd, when string) {
	for _, path := range modelFiles(cmd) {
		evicted, measured, err := dropPageCache(path)
		switch {
		case err != nil:
			slog.Warn("page cache: could not drop model file "+when, "file", path, "err", err)
		case measured:
			slog.Info("page cache: dropped model file "+when, "file", path, "evictedBytes", evicted)
		}
	}
}

// AFTER the backend is ready, the model files are dropped again.
//
// Loading reads the whole file through the page cache, and with every layer on
// the GPU llama.cpp unmaps it afterwards, so those pages are cache nobody uses:
// ~27 GB for Swift on box1, charged to the backend's cgroup. On 2026-11-05 that
// cache plus ~43 GiB of pinned prompt cache left too little room for other
// services' bursts; session memory pressure reached 90 % and systemd-oomd
// killed two of them, and a manual memory.reclaim of exactly this cache took
// pressure back to ~0. A file a live process still maps keeps its pages
// (DONTNEED does not touch them), so a CPU-offloaded model is unaffected.

// evictAfterReady is the drop itself; a var so a test can observe it.
var evictAfterReady = func(cmd string) { evictPageCache(cmd, "after ready") }

// readyPoll is how often the post-ready drop asks the backend for /health, and
// readyWait how long it keeps asking. Vars for tests.
var (
	readyPoll = 2 * time.Second
	readyWait = 30 * time.Minute
)

// evictWhenReady waits for http://127.0.0.1:port/health to answer 2xx, then
// drops the model files cmd names from the page cache. It gives up when done
// closes (the backend exited) or after readyWait. Best effort, like the drop
// before spawn: nothing here can fail or delay a backend.
func evictWhenReady(port int, cmd string, done <-chan struct{}) {
	if port <= 0 || len(modelFiles(cmd)) == 0 {
		return
	}
	client := &http.Client{Timeout: time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(readyWait)
	tick := time.NewTicker(readyPoll)
	defer tick.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			evictAfterReady(cmd)
			return
		}
	}
}

// modelFiles lists the existing regular model files a shell command names, in
// order, without duplicates: each word, and the value of each --flag=value
// word, that has a model extension — plus every sibling shard of a split gguf,
// since llama.cpp loads all of them from the first.
func modelFiles(cmd string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if seen[p] {
			return
		}
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, w := range shellWords(cmd) {
		cands := []string{w}
		if _, v, ok := strings.Cut(w, "="); ok {
			cands = append(cands, v)
		}
		for _, c := range cands {
			c = expandHome(os.ExpandEnv(c))
			if !hasModelExt(c) {
				continue
			}
			add(c)
			for _, s := range shardSiblings(c) {
				add(s)
			}
		}
	}
	return out
}

// shellWords splits a command the way ParseLaunch does: backslash-newline
// continuations joined, # comments dropped, simple quotes removed. Enough to
// find file paths; it is not a shell.
func shellWords(cmd string) []string {
	var words []string
	for line := range strings.SplitSeq(strings.ReplaceAll(cmd, "\\\n", " "), "\n") {
		for i, r := range line {
			if r == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
				line = line[:i]
				break
			}
		}
		for w := range strings.FieldsSeq(line) {
			words = append(words, strings.Trim(w, `"'`))
		}
	}
	return words
}

func hasModelExt(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	for _, e := range modelExts {
		if ext == e {
			return true
		}
	}
	return false
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// shardSiblings names every shard of the split gguf p is one of (p included),
// or nil when p is not a shard.
func shardSiblings(p string) []string {
	m := reShard.FindStringSubmatch(filepath.Base(p))
	if m == nil {
		return nil
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n < 1 || n > 9999 {
		return nil
	}
	dir := filepath.Dir(p)
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, filepath.Join(dir, fmt.Sprintf("%s-%05d-of-%s.gguf", m[1], i, m[3])))
	}
	return out
}
