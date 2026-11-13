package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\n\nDOTENV_A=plain\nexport DOTENV_B=\"quoted value\"\nDOTENV_C='single'\nDOTENV_SET=from-file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"DOTENV_A", "DOTENV_B", "DOTENV_C"} {
		t.Setenv(k, "") // registers cleanup; then unset so the file can fill it
		os.Unsetenv(k)
	}
	t.Setenv("DOTENV_SET", "from-env")

	if err := loadDotEnv(path, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DOTENV_A":   "plain",
		"DOTENV_B":   "quoted value",
		"DOTENV_C":   "single",
		"DOTENV_SET": "from-env", // the real environment wins over the file
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestLoadDotEnvMissingFileIsFine(t *testing.T) {
	if err := loadDotEnv(filepath.Join(t.TempDir(), "nope"), slog.New(slog.DiscardHandler)); err != nil {
		t.Errorf("missing file: %v", err)
	}
}

func TestLoadDotEnvRejectsMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("NOT_A_PAIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotEnv(path, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("expected an error for a line without '='")
	}
}
