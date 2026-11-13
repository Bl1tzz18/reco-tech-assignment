package main

import (
	"strings"
	"testing"
)

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("defaults should be valid: %v", err)
	}
	if cfg.baseURL != defaultBaseURL || cfg.rpm != defaultRPM || cfg.pageSize != defaultPageSize {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestParseFlagsReportsAllProblems(t *testing.T) {
	_, err := parseFlags([]string{"-rpm", "0", "-page-size", "500", "-base-url", "ftp://x", "-timeout", "0s"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"-rpm", "-page-size", "-base-url", "-timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
