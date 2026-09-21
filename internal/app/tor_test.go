package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGeneratedConfigurationAcceptedByTor(t *testing.T) {
	if _, err := exec.LookPath("tor"); err != nil {
		t.Skip("Tor is installed in the Docker test stage")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "torrc")
	if err := os.WriteFile(path, []byte(torConfiguration(filepath.Join(dir, "data with spaces"), filepath.Join(dir, "control"), filepath.Join(dir, "socks"))), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("tor", "--verify-config", "-f", path).CombinedOutput()
	if err != nil {
		t.Fatalf("Tor rejected generated configuration: %v\n%s", err, output)
	}
}
