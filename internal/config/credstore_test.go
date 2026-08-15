package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCreds(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, CredentialsFileName)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		secretsMu.Lock()
		secrets = nil
		secretsMu.Unlock()
	})
	return dir
}

// TestCredentialsResolveThroughEnvRefs: config keeps holding a REFERENCE, and
// the value comes from the store. No schema change, nothing to migrate, and the
// document /api/v1/config/* serves still contains no secret.
func TestCredentialsResolveThroughEnvRefs(t *testing.T) {
	dir := writeCreds(t, "OPENROUTER_KEY_WORK=sk-or-secret\n", 0o600)
	if err := LoadCredentials(dir); err != nil {
		t.Fatal(err)
	}
	if got := lookupSecret("OPENROUTER_KEY_WORK"); got != "sk-or-secret" {
		t.Errorf("lookupSecret = %q", got)
	}
}

// TestCredentialsBeatEnvironment: the store is the deliberate, managed source.
// Env-first would let an ambient variable silently win over a value someone
// just typed into the file (or a UI), with no way to tell why.
func TestCredentialsBeatEnvironment(t *testing.T) {
	t.Setenv("P21_SHADOWED", "from-env")
	dir := writeCreds(t, "P21_SHADOWED=from-file\n", 0o600)
	if err := LoadCredentials(dir); err != nil {
		t.Fatal(err)
	}
	if got := lookupSecret("P21_SHADOWED"); got != "from-file" {
		t.Errorf("lookupSecret = %q, want the file to win", got)
	}
}

// TestCredentialsFallBackToEnvironment: a key the store does not carry still
// resolves, so every deployment predating this file keeps working untouched.
func TestCredentialsFallBackToEnvironment(t *testing.T) {
	t.Setenv("P21_ONLY_ENV", "from-env")
	dir := writeCreds(t, "SOMETHING_ELSE=x\n", 0o600)
	if err := LoadCredentials(dir); err != nil {
		t.Fatal(err)
	}
	if got := lookupSecret("P21_ONLY_ENV"); got != "from-env" {
		t.Errorf("lookupSecret = %q, want the environment fallback", got)
	}
}

// TestMissingCredentialsFileIsFine — the additive contract.
func TestMissingCredentialsFileIsFine(t *testing.T) {
	if err := LoadCredentials(t.TempDir()); err != nil {
		t.Errorf("a missing credential store must not be an error: %v", err)
	}
}

// TestCredentialsRefuseLoosePermissions: refused like an ssh private key rather
// than loaded with a warning nobody reads. Failing is loud and fixable; loading
// is silent and permanent.
func TestCredentialsRefuseLoosePermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o644, 0o604, 0o666} {
		dir := writeCreds(t, "K=v\n", mode)
		err := LoadCredentials(dir)
		if err == nil {
			t.Errorf("mode %04o: want a refusal, got nil", mode)
			continue
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: the error must say how to fix it, got %q", mode, err)
		}
		if lookupSecret("K") != "" {
			t.Errorf("mode %04o: secrets were loaded despite the refusal", mode)
		}
	}
}

// TestSecretNamesLeaksNoValues: diagnostics may list keys, never values — a
// getter is how a secret ends up in a log line.
func TestSecretNamesLeaksNoValues(t *testing.T) {
	dir := writeCreds(t, "A=super-secret\nB=other\n", 0o600)
	if err := LoadCredentials(dir); err != nil {
		t.Fatal(err)
	}
	names := SecretNames()
	if len(names) != 2 {
		t.Fatalf("SecretNames = %v", names)
	}
	for _, n := range names {
		if strings.Contains(n, "super-secret") || strings.Contains(n, "other") {
			t.Errorf("SecretNames returned a value: %q", n)
		}
	}
}

// TestSetSecretWritesAndResolves: a value set through the API must be usable by
// ${...} expansion immediately, not after a restart.
func TestSetSecretWritesAndResolves(t *testing.T) {
	dir := writeCreds(t, "EXISTING=old\n", 0o600)
	if err := LoadCredentials(dir); err != nil {
		t.Fatal(err)
	}
	if err := SetSecret("NEWKEY", "sk-new"); err != nil {
		t.Fatal(err)
	}
	if got := lookupSecret("NEWKEY"); got != "sk-new" {
		t.Errorf("lookupSecret = %q, want it live immediately", got)
	}
	// and it survives a reload from disk
	secretsMu.Lock()
	secrets = nil
	secretsMu.Unlock()
	if err := LoadCredentials(dir); err != nil {
		t.Fatal(err)
	}
	if got := lookupSecret("NEWKEY"); got != "sk-new" {
		t.Errorf("after reload lookupSecret = %q", got)
	}
	if got := lookupSecret("EXISTING"); got != "old" {
		t.Errorf("writing one key dropped another: EXISTING = %q", got)
	}
}

// TestSetSecretKeepsFileMode: the store is rewritten whole, and corrallm
// refuses to start on a loose mode — so a write that widened it would break the
// next restart.
func TestSetSecretKeepsFileMode(t *testing.T) {
	dir := writeCreds(t, "A=1\n", 0o600)
	if err := LoadCredentials(dir); err != nil {
		t.Fatal(err)
	}
	if err := SetSecret("B", "2"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, CredentialsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("mode widened to %04o — the next startup would refuse to load it", perm)
	}
}

// TestSetSecretEmptyDeletes: clearing a value removes the key rather than
// storing an empty string that would expand to "" and look like a broken token.
func TestSetSecretEmptyDeletes(t *testing.T) {
	dir := writeCreds(t, "GONE=x\n", 0o600)
	if err := LoadCredentials(dir); err != nil {
		t.Fatal(err)
	}
	if err := SetSecret("GONE", ""); err != nil {
		t.Fatal(err)
	}
	for _, n := range SecretNames() {
		if n == "GONE" {
			t.Error("cleared secret still present")
		}
	}
}

// TestSetSecretRejectsUnreferenceableName: config refers to a secret as ${NAME}
// and envRef only matches that shape, so a name outside it would be stored and
// never usable.
func TestSetSecretRejectsUnreferenceableName(t *testing.T) {
	dir := writeCreds(t, "", 0o600)
	if err := LoadCredentials(dir); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "has space", "has-dash", "1leading", "dot.name"} {
		if err := SetSecret(bad, "v"); err == nil {
			t.Errorf("name %q should be refused as unreferenceable", bad)
		}
	}
}
