package recipes

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNamesExcludesTheSharedLibrary(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("no recipes embedded; the //go:embed pattern matched nothing")
	}
	for _, n := range names {
		if n == "common" {
			t.Error("common.sh is the shared helper library, not a recipe — it must never be offered as a tool")
		}
	}
}

func TestTheShippedToolsHaveRecipes(t *testing.T) {
	for _, want := range []string{"llama.cpp", "ninfer", "stable-diffusion.cpp", "nemo-speech.cpp"} {
		if !Has(want) {
			t.Errorf("no recipe for %q (have: %v)", want, Names())
		}
	}
	if Has("common") {
		t.Error("Has(\"common\") must be false — see TestNamesExcludesTheSharedLibrary")
	}
	if Has("") || Has("nope") {
		t.Error("Has must reject names with no recipe")
	}
}

// Extract must write the WHOLE set, not just one file: every recipe sources
// common.sh from its own directory, so extracting one alone produces a script
// that cannot start.
func TestExtractWritesTheSharedLibraryToo(t *testing.T) {
	dir := t.TempDir()
	if err := Extract(dir); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "common.sh")); err != nil {
		t.Fatalf("common.sh not extracted: %v — every recipe sources it and would fail to start", err)
	}
	for _, n := range Names() {
		fi, err := os.Stat(filepath.Join(dir, n+".sh"))
		if err != nil {
			t.Fatalf("recipe %s not extracted: %v", n, err)
		}
		if fi.Mode().Perm()&0o100 == 0 {
			t.Errorf("recipe %s is not executable by its owner (mode %v)", n, fi.Mode().Perm())
		}
		if fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("recipe %s is group/world writable (mode %v) — anyone on the box could rewrite what the agent runs as itself", n, fi.Mode().Perm())
		}
	}
}

func TestExtractIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := range 2 {
		if err := Extract(dir); err != nil {
			t.Fatalf("Extract #%d: %v", i+1, err)
		}
	}
}

// Every recipe must answer the read-only verbs. A recipe that silently lacks one
// would report as a runtime error on a host, which is the worst place to find out.
func TestEveryRecipeHandlesTheReadOnlyVerbs(t *testing.T) {
	for _, n := range Names() {
		b, err := Script(n)
		if err != nil {
			t.Fatalf("Script(%s): %v", n, err)
		}
		src := string(b)
		// builds/activate are here too: a recipe that silently lacks them
		// leaves its tool with no way back off a bad build, which is only
		// discovered when somebody needs one.
		for _, verb := range []string{"probe)", "upstream)", "preflight)", "install-deps)", "builds)", "activate)"} {
			if !strings.Contains(src, verb) {
				t.Errorf("recipe %s has no %s case", n, strings.TrimSuffix(verb, ")"))
			}
		}
	}
}

// The output contract: the JSON result goes to stdout and everything else to
// stderr. A recipe that echoes progress to stdout corrupts its own answer, and
// the failure looks like a parse error rather than a stray echo.
func TestRecipesDoNotEchoToStdout(t *testing.T) {
	for _, n := range Names() {
		b, err := Script(n)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "echo ") {
				continue
			}
			if !strings.Contains(trimmed, ">&2") {
				t.Errorf("%s.sh:%d echoes to stdout, which corrupts the JSON result: %s", n, i+1, trimmed)
			}
		}
	}
}

// Extraction races execution, and the failure is intermittent.
//
// A survey extracts from several goroutines at once while other recipes are
// already running from that same directory. With a truncate-then-write, bash
// occasionally opens a half-written script; it surfaced as a probe returning
// unparseable output, which reads like a broken recipe rather than a torn file.
// This runs the two against each other hard enough to catch a non-atomic write.
func TestExtractIsSafeWhileScriptsAreRead(t *testing.T) {
	dir := t.TempDir()
	if err := Extract(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "common.sh")
	want, err := files.ReadFile("common.sh")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var torn atomic.Int64

	// Readers: a reader must never observe a partial file.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := os.ReadFile(target)
				// A rename can briefly make the path absent on some systems;
				// that is recoverable and not what this is looking for. A
				// SHORT file is the bug.
				if err == nil && len(got) != len(want) {
					torn.Add(1)
				}
			}
		}()
	}
	// Writers: several concurrent extractions, as a survey produces.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				if err := Extract(dir); err != nil {
					t.Errorf("Extract: %v", err)
					return
				}
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	if n := torn.Load(); n > 0 {
		t.Errorf("observed %d partially-written reads — extraction is not atomic", n)
	}
}

// Probe parsing, run against a fake binary on each recipe.
//
// THE TWO TOOLS DISAGREE ABOUT WHICH STREAM A VERSION GOES TO, which is exactly
// the class of mistake that produces "version unknown" on a working binary and
// is invisible until somebody looks at a real host. llama-server writes its
// banner to STDERR and exits 0; sd-server writes its to STDOUT. A recipe that
// copies the other's handling parses nothing, and nothing else in this suite
// would notice — the script is still valid bash and still prints valid JSON.
//
// So the fake binaries here print to the stream the real ones print to, and
// they are deliberately kept in ONE table: the next recipe added is meant to
// have to answer this question rather than inherit an answer.
func TestProbeReadsTheVersionEachToolActuallyPrints(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash; the recipes are bash and cannot be exercised here")
	}
	dir := t.TempDir()
	if err := Extract(dir); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name        string
		recipe      string
		bin         string
		script      string
		wantPresent bool
		wantVersion string
		wantCommit  string
	}{
		{
			name:   "llama-server prints to stderr",
			recipe: "llama.cpp",
			bin:    "llama-server",
			script: "#!/bin/sh\n" +
				"echo 'version: 10380 (0b1bad14f)' >&2\n" +
				"echo 'built with Clang 18.1.3 for Linux x86_64' >&2\n",
			wantPresent: true,
			wantVersion: "10380 (0b1bad14f)",
			wantCommit:  "0b1bad14f",
		},
		{
			name:   "sd-server prints to stdout",
			recipe: "stable-diffusion.cpp",
			bin:    "sd-server",
			script: "#!/bin/sh\n" +
				"echo 'stable-diffusion.cpp version master-c678dfe-12-gc678dfe7, commit c678dfe'\n",
			wantPresent: true,
			wantVersion: "master-c678dfe-12-gc678dfe7",
			wantCommit:  "c678dfe",
		},
		{
			// sd.cpp's cmake runs `git describe --tags` with ERROR_QUIET and
			// bakes in the literal "unknown" when it finds no tag, so a tagless
			// tree yields a real commit beside a useless version. Reporting
			// "unknown" would hide the one fact the binary does know.
			name:   "sd-server with no describable tag falls back to its commit",
			recipe: "stable-diffusion.cpp",
			bin:    "sd-server",
			script: "#!/bin/sh\n" +
				"echo 'stable-diffusion.cpp version unknown, commit c678dfe'\n",
			wantPresent: true,
			wantVersion: "c678dfe",
			wantCommit:  "c678dfe",
		},
		{
			// A patched tree reports its own dirt: `--dirty=+` is part of the
			// describe sd.cpp bakes in, so corrallm's patch set shows up in the
			// version without the stamp being consulted.
			name:   "sd-server keeps the dirty marker a patched tree earns",
			recipe: "stable-diffusion.cpp",
			bin:    "sd-server",
			script: "#!/bin/sh\n" +
				"echo 'stable-diffusion.cpp version master-c678dfe+, commit c678dfe'\n",
			wantPresent: true,
			wantVersion: "master-c678dfe+",
			wantCommit:  "c678dfe",
		},
		{
			// A THIRD ANSWER to the same question. nemo-speech prints to
			// stdout like sd-server, but its banner carries NO COMMIT at all:
			// CMakeLists.txt parses the VERSION file and bakes the string in,
			// with no `git describe` anywhere. A recipe that copied sd.cpp's
			// two-part parse would report an empty version on a working binary.
			name:   "nemo-speech prints a bare release to stdout and no commit",
			recipe: "nemo-speech.cpp",
			bin:    "nemo-speech",
			script: "#!/bin/sh\n" +
				"echo 'nemo-speech 0.1.0'\n",
			wantPresent: true,
			wantVersion: "0.1.0",
			wantCommit:  "", // nothing to read; the stamp is the only source
		},
		{
			name:        "nothing installed",
			recipe:      "stable-diffusion.cpp",
			bin:         "sd-server",
			script:      "",
			wantPresent: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := t.TempDir()
			if tc.script != "" {
				bindir := filepath.Join(prefix, "bin")
				if err := os.MkdirAll(bindir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(bindir, tc.bin), []byte(tc.script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", filepath.Join(dir, tc.recipe+".sh"), "probe")
			cmd.Env = append(os.Environ(),
				"TOOL_NAME="+tc.recipe,
				"TOOL_PREFIX="+prefix,
				"TOOL_BIN="+tc.bin,
				// Emptied explicitly: an adopted install reads a directory
				// elsewhere, and inheriting one from the environment would
				// point this at a path that is not the fixture.
				"TOOL_INSTALLED_AT=",
			)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			var got struct {
				Present bool   `json:"present"`
				Version string `json:"version"`
				Commit  string `json:"commit"`
				Source  string `json:"source"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("probe printed unparseable JSON %q: %v", out, err)
			}
			if got.Present != tc.wantPresent {
				t.Fatalf("present = %v, want %v (%q)", got.Present, tc.wantPresent, out)
			}
			if !tc.wantPresent {
				return
			}
			if got.Version != tc.wantVersion {
				t.Errorf("version = %q, want %q", got.Version, tc.wantVersion)
			}
			if got.Commit != tc.wantCommit {
				t.Errorf("commit = %q, want %q", got.Commit, tc.wantCommit)
			}
			if got.Source != "binary" {
				t.Errorf("source = %q, want \"binary\" — the binary answered, so the stamp must not be credited", got.Source)
			}
		})
	}
}

// The three ways sd.cpp's build is not llama.cpp's.
//
// Each is checked against the script rather than a host because the alternative
// is a twenty-minute build that fails at configure, and each has a failure that
// looks like something else:
//
//	ggml is a SUBMODULE — a plain clone leaves it empty and cmake/ggml.cmake
//	dies in a directory nobody knew was involved. align_tree only clones and
//	resets, so the recipe has to do this itself.
//
//	examples/server/frontend is an UNINITIALISED submodule, which still leaves
//	an empty DIRECTORY on disk. sd.cpp's cmake tests only that the directory
//	exists before shelling out to pnpm — and the Mac has pnpm, because it
//	builds corrallm's own UI.
//
//	EVERY backend is off by default, where llama.cpp's cmake turns Metal on for
//	Apple with no flag from us.
func TestStableDiffusionBuildAnswersTheThingsLlamaCppDoesNotHaveToo(t *testing.T) {
	b, err := Script("stable-diffusion.cpp")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []struct{ needle, why string }{
		{"git submodule update --init",
			"the recipe never updates a submodule — sd.cpp vendors ggml as one and cannot configure without it"},
		{"-DSD_SERVER_BUILD_FRONTEND=OFF",
			"the recipe does not disable sd-server's frontend build — on a host with pnpm, cmake runs `pnpm install` in the empty frontend submodule and the build fails"},
		{"-DSD_METAL=ON",
			"the recipe never passes -DSD_METAL=ON — sd.cpp defaults every backend OFF, so an Apple build without it is CPU-only and looks like a GPU one"},
	} {
		if !strings.Contains(src, want.needle) {
			t.Error(want.why)
		}
	}
}

// The stamp is the ONLY thing that identifies a NeMo-Speech.cpp build.
//
// Its banner is `nemo-speech 0.1.0` and always will be — upstream has published
// no release past v0.1.0 and the string comes from a checked-in VERSION file,
// not from `git describe`. So a tree at the pinned commit and a tree months
// ahead of it answer identically, and everything drift-shaped (`tools
// upstream`, the scheduled check) reads the commit out of the stamp instead.
//
// Checked here rather than on a host because the failure is invisible: a recipe
// that dropped the stamp would still print a version, still print valid JSON,
// and would simply never report drift again.
func TestNemoSpeechTakesItsCommitFromTheStampBecauseTheBinaryHasNone(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash; the recipes are bash and cannot be exercised here")
	}
	dir := t.TempDir()
	if err := Extract(dir); err != nil {
		t.Fatal(err)
	}

	prefix := t.TempDir()
	bindir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bindir, "nemo-speech"),
		[]byte("#!/bin/sh\necho 'nemo-speech 0.1.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	const head = "97a15af0d4f6b1c2e3a4b5c6d7e8f90123456789"
	if err := os.WriteFile(filepath.Join(bindir, ".corrallm-stamp"),
		[]byte("head="+head+" patches=none archs=cuda:86-real"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", filepath.Join(dir, "nemo-speech.cpp.sh"), "probe")
	cmd.Env = append(os.Environ(),
		"TOOL_NAME=nemo-speech.cpp", "TOOL_PREFIX="+prefix,
		"TOOL_BIN=nemo-speech", "TOOL_INSTALLED_AT=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	var got struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("probe printed unparseable JSON %q: %v", out, err)
	}
	if got.Version != "0.1.0" {
		t.Errorf("version = %q, want %q — the release string the binary prints", got.Version, "0.1.0")
	}
	if got.Commit != head {
		t.Errorf("commit = %q, want %q — the stamp is the only source of it", got.Commit, head)
	}
	if got.Source != "binary" {
		t.Errorf("source = %q, want \"binary\": the version came from the binary even though the commit did not", got.Source)
	}
}

// The ways NeMo-Speech.cpp's build is not stable-diffusion.cpp's.
//
// Each is checked against the script rather than a host, because the
// alternative is a four-and-a-half-minute CUDA build plus a SentencePiece
// compile and two submodule clones, and each failure looks like something else:
//
//	THE TREE PATCHES ITS OWN ggml SUBMODULE (23 patches, in-tree, applied to
//	the worktree). A dirty submodule makes the superproject's `git diff` report
//	a tracked change, which align_tree refuses to move — so without an explicit
//	reset BEFORE the align, the second build freezes the checkout permanently
//	while every verb keeps answering normally.
//
//	PTX IS UNUSABLE on a box whose driver is older than its only toolkit: the
//	first kernel that JITs dies with "the provided PTX was compiled with an
//	unsupported toolchain". -real emits SASS and nothing to JIT. llama.cpp's
//	recipe passes the bare arch list on the same box and is right to, so this
//	cannot be inherited — it has to be decided here.
//
//	scripts/configure.sh DEMANDS THE llama.cpp SUBMODULE for every ASR preset,
//	for one header that only NEMO_SPEECH_BUILD_MIC_CAPTURE compiles. The recipe
//	drives cmake --preset itself to avoid a large clone it will not use.
//
//	SENTENCEPIECE IS NOT VENDORED and src/asr/CMakeLists.txt makes it REQUIRED.
//	Upstream's helper defaults its prefix to <src>/.deps, which `git clean -xdf`
//	deletes — so it must be built somewhere the clean does not reach.
//
//	cpp-httplib IS A SEPARATE SUBMODULE and the configure fails by name without
//	it as soon as NEMO_SPEECH_BUILD_HTTP is on, which for corrallm is always.
func TestNemoSpeechBuildAnswersTheThingsStableDiffusionDoesNotHaveTo(t *testing.T) {
	b, err := Script("nemo-speech.cpp")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []struct{ needle, why string }{
		{"reset_vendored_ggml",
			"the recipe never restores the vendored ggml worktree — the in-tree patch series leaves it dirty, align_tree then refuses to move the tree, and the checkout freezes at the patched commit from the second build onward"},
		{"scripts/apply-ggml-patches.sh",
			"the recipe never runs the tree's own ggml series — the CUDA preset sets NEMO_SPEECH_GGML_PATCHED=ON and compiles against ops that are not there"},
		{"-real",
			"the recipe never asks for SASS-only CUDA architectures — with PTX in the binary, a driver older than the toolkit dies at the first JIT with \"the provided PTX was compiled with an unsupported toolchain\""},
		{"-DNEMO_SPEECH_BUILD_HTTP=ON",
			"the recipe never turns the HTTP server on — corrallm proxies /v1/audio/transcriptions and there is nothing to proxy without it"},
		{"-DNEMO_SPEECH_BUILD_MIC_CAPTURE=OFF",
			"the recipe never turns mic capture off — it is the only ASR-side consumer of llama.cpp/vendor/miniaudio, and leaving it on makes a large submodule mandatory for a header this build does not need"},
		{"third_party/cpp-httplib",
			"the recipe never checks out cpp-httplib — CMakeLists.txt fails the configure by name once NEMO_SPEECH_BUILD_HTTP is on"},
		{"build_sentencepiece",
			"the recipe never builds SentencePiece — src/asr/CMakeLists.txt makes it REQUIRED and nothing in the tree vendors it"},
		{"NEMO_SPEECH_DEPENDENCY_PREFIX",
			"the recipe never points cmake at the SentencePiece it built — upstream's default prefix is <src>/.deps, which the build's own `git clean -xdf` deletes, so it would be recompiled every time"},
		{"cmake --preset",
			"the recipe does not drive cmake --preset itself — scripts/configure.sh demands the llama.cpp submodule for every ASR preset, for a header this build does not compile"},
	} {
		if !strings.Contains(src, want.needle) {
			t.Error(want.why)
		}
	}

	// Ordering is load-bearing and a strings.Contains cannot see it: the ggml
	// reset has to happen BEFORE align_tree, or align_tree is the thing that
	// refuses. Compare positions inside build().
	at := strings.Index(src, "\nbuild() {")
	if at < 0 {
		t.Fatal("no build() in the recipe")
	}
	body := src[at:]
	reset := strings.Index(body, "reset_vendored_ggml \"$src\"")
	align := strings.Index(body, "align_tree \"$src\"")
	if reset < 0 || align < 0 {
		t.Fatal("build() no longer calls both reset_vendored_ggml and align_tree by name")
	}
	if reset > align {
		t.Error("build() resets the vendored ggml AFTER align_tree — by then align_tree has already refused to move a tree with tracked changes, warned once, and left the checkout frozen at the patched commit")
	}
}
