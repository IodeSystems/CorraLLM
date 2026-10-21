package host

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Which words of a command are model files: plain paths and --flag=path
// values with a model extension that exist as regular files, plus every shard
// of a split gguf. The binary, a chat template, a missing file and a
// directory named like a model are not.
func TestModelFiles(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "Qwen-Q6_K.gguf")
	proj := filepath.Join(dir, "mmproj-F16.gguf")
	draft := filepath.Join(dir, "draft.safetensors")
	bin := filepath.Join(dir, "lora.BIN")
	tmpl := filepath.Join(dir, "chat.jinja")
	server := filepath.Join(dir, "llama-server")
	for _, p := range []string{main, proj, draft, bin, tmpl, server} {
		touch(t, p)
	}
	shard := func(i int) string {
		return filepath.Join(dir, "Big-Q8_0-0000"+string(rune('0'+i))+"-of-00003.gguf")
	}
	for i := 1; i <= 3; i++ {
		touch(t, shard(i))
	}
	if err := os.Mkdir(filepath.Join(dir, "adir.gguf"), 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := server + " -m " + main + " \\\n --mmproj=" + proj +
		" --model-draft '" + draft + "' --lora " + bin +
		" --chat-template-file " + tmpl +
		" -m " + filepath.Join(dir, "missing.gguf") +
		" --x=" + filepath.Join(dir, "adir.gguf") +
		" -m " + shard(2) +
		" -m " + main + // a repeat is listed once
		" # -m " + filepath.Join(dir, "commented.gguf")
	touch(t, filepath.Join(dir, "commented.gguf"))

	got := modelFiles(cmd)
	want := []string{main, proj, draft, bin, shard(1), shard(2), shard(3)}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("modelFiles:\n got %v\nwant %v", got, want)
	}
}

func TestShardSiblings(t *testing.T) {
	got := shardSiblings("/m/Big-Q8_0-00002-of-00003.gguf")
	want := []string{"/m/Big-Q8_0-00001-of-00003.gguf", "/m/Big-Q8_0-00002-of-00003.gguf", "/m/Big-Q8_0-00003-of-00003.gguf"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if s := shardSiblings("/m/Small-Q8_0.gguf"); s != nil {
		t.Errorf("not a shard: got %v", s)
	}
}

// Read a file into the cache, then drop it. Only that nothing fails is
// asserted: a tmpfs TempDir may ignore the advice entirely.
func TestDropPageCacheRuns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "w.gguf")
	if err := os.WriteFile(p, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dropPageCache(p); err != nil {
		t.Fatalf("dropPageCache: %v", err)
	}
	EvictPageCache("llama-server -m " + p) // the logging wrapper, same file
}
