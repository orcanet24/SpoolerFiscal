package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvLoadsValuesWithoutReplacingExistingEnvironment(t *testing.T) {
	t.Setenv("SPOOLER_DOTENV_EXISTING_TEST", "system")
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\nSPOOLER_DOTENV_NEW_TEST=from-file\nSPOOLER_DOTENV_EXISTING_TEST=file\nSPOOLER_DOTENV_QUOTED_TEST=\"two words\"\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	if err := LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got := os.Getenv("SPOOLER_DOTENV_NEW_TEST"); got != "from-file" {
		t.Fatalf("new value = %q, want from-file", got)
	}
	if got := os.Getenv("SPOOLER_DOTENV_EXISTING_TEST"); got != "system" {
		t.Fatalf("existing value = %q, want system", got)
	}
	if got := os.Getenv("SPOOLER_DOTENV_QUOTED_TEST"); got != "two words" {
		t.Fatalf("quoted value = %q, want %q", got, "two words")
	}
}

func TestLoadDotEnvRejectsMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("INVALID_LINE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadDotEnv(path); err == nil {
		t.Fatal("LoadDotEnv accepted malformed line")
	}
}
