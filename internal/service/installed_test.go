package service

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFindInstalled(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("corrallm.service", Unit{Exec: "/x/corrallm"}.Render())
	write("corrallm-agent.service", Unit{Name: DefaultAgentName, Exec: "/x/corrallm", Agent: true}.Render())
	write("corrallm-lab.service", Unit{Name: "corrallm-lab", Exec: "/x/corrallm"}.Render())
	write("other.service", "[Unit]\nDescription=not ours\n")
	got, err := FindInstalled(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []Installed{{Name: "corrallm"}, {Name: "corrallm-lab"}, {Name: "corrallm-agent", Agent: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("systemd: got %+v, want %+v", got, want)
	}

	ld := t.TempDir()
	for _, u := range []Unit{{Exec: "/x/corrallm"}, {Name: DefaultAgentName, Exec: "/x/corrallm", Agent: true}} {
		if _, err := u.InstallPlist(ld, filepath.Join(ld, "l", "x.log")); err != nil {
			t.Fatal(err)
		}
	}
	got, err = FindInstalled(ld, true)
	if err != nil {
		t.Fatal(err)
	}
	want = []Installed{{Name: "corrallm"}, {Name: "corrallm-agent", Agent: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("launchd: got %+v, want %+v", got, want)
	}
}
