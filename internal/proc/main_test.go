package proc

import (
	"os"
	"testing"

	"github.com/iodesystems/corrallm/internal/host"
)

// TestMain: these tests stand a fake backend up on the model's port before the
// spawn (the spawned cmd is `sleep`), which host's port-in-use check refuses.
func TestMain(m *testing.M) {
	host.CheckPortFree = false
	os.Exit(m.Run())
}
