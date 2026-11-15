package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProtectConfigFileKeepsItReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte("station_id: CAJA-01\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ProtectConfigFile(path); err != nil {
		t.Fatalf("ProtectConfigFile: %v", err)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatalf("el archivo quedó ilegible tras restringir permisos: %v", err)
	}
}

func TestProtectDataDirCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := ProtectDataDir(dir); err != nil {
		t.Fatalf("ProtectDataDir: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("no se creó el directorio: %v", err)
	}
}
