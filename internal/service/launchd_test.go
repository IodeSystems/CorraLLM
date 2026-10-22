package service

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestPlistIsWellFormed: launchd silently ignores a plist it cannot parse, so
// a stray & in a path would leave a job that never loads and says nothing.
func TestPlistIsWellFormed(t *testing.T) {
	u := Unit{Exec: "/Users/x/.local/bin/corrallm", Args: []string{"--config", "/a&b/<c>.yml"},
		WorkingDir: "/Users/x/.corrallm", Env: []string{"ADDR=0.0.0.0:8111", "PATH=/opt/homebrew/bin:/usr/bin"}}
	got := u.RenderPlist("/Users/x/Library/Logs/corrallm/corrallm.log")
	d := xml.NewDecoder(strings.NewReader(got))
	d.Strict = true
	for {
		_, err := d.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, got)
		}
	}
	for _, want := range []string{
		"<string>com.iodesystems.corrallm</string>",
		"<string>serve</string>",
		"<key>ADDR</key>\n\t\t<string>0.0.0.0:8111</string>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ExitTimeOut</key>\n\t<integer>90</integer>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AbandonProcessGroup") {
		t.Error("serve must not abandon its process group: it reaps its own backends")
	}
}

// TestPlistAgentLeavesBackendsRunning: the launchd form of KillMode=process.
func TestPlistAgentLeavesBackendsRunning(t *testing.T) {
	u := Unit{Name: DefaultAgentName, Exec: "/x/corrallm", Agent: true, Args: []string{"--config", "/a.yml"}}
	got := u.RenderPlist("/l.log")
	for _, want := range []string{"<string>agent</string>", "<key>AbandonProcessGroup</key>\n\t<true/>", "com.iodesystems.corrallm-agent"} {
		if !strings.Contains(got, want) {
			t.Errorf("agent plist is missing %q:\n%s", want, got)
		}
	}
}

// TestPlistArgsRoundTrip: restart finds --config in the installed job.
func TestPlistArgsRoundTrip(t *testing.T) {
	u := Unit{Exec: "/x/corrallm", Args: []string{"--config", "/a & b/c.yml", "--web-root", "/w"}}
	got := PlistArgs(u.RenderPlist("/l.log"))
	want := []string{"/x/corrallm", "serve", "--config", "/a & b/c.yml", "--web-root", "/w"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PlistArgs = %q, want %q", got, want)
	}
}

func TestInstallPlistRefusesRelativeExec(t *testing.T) {
	dir := t.TempDir()
	if _, err := (Unit{Exec: "corrallm"}).InstallPlist(dir, filepath.Join(dir, "l", "x.log")); err == nil {
		t.Fatal("relative exec accepted")
	}
	p, err := (Unit{Exec: "/x/corrallm"}).InstallPlist(dir, filepath.Join(dir, "l", "x.log"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != "com.iodesystems.corrallm.plist" {
		t.Errorf("installed as %s", p)
	}
	if _, err := os.Stat(filepath.Join(dir, "l")); err != nil {
		t.Errorf("log dir not created: %v", err)
	}
}
