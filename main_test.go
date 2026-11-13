package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("defaults should be valid: %v", err)
	}
	if cfg.baseURL != defaultBaseURL || cfg.rpm != defaultRPM || cfg.pageSize != defaultPageSize {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if cfg.usersInterval != defaultInterval || cfg.projectsInterval != defaultInterval || cfg.once {
		t.Errorf("default should poll everything every %s: %+v", defaultInterval, cfg)
	}
}

func TestParseFlagsIntervals(t *testing.T) {
	tests := []struct {
		args            []string
		users, projects time.Duration
	}{
		{[]string{"-interval", "30s"}, 30 * time.Second, 30 * time.Second},
		{[]string{"-interval", "5m"}, 5 * time.Minute, 5 * time.Minute},
		{[]string{"-users-interval", "30s"}, 30 * time.Second, defaultInterval},
		{[]string{"-interval", "1m", "-projects-interval", "10m"}, time.Minute, 10 * time.Minute},
	}
	for _, tt := range tests {
		cfg, err := parseFlags(tt.args)
		if err != nil {
			t.Fatalf("%v: %v", tt.args, err)
		}
		if cfg.usersInterval != tt.users || cfg.projectsInterval != tt.projects {
			t.Errorf("%v: users=%s projects=%s, want %s %s", tt.args, cfg.usersInterval, cfg.projectsInterval, tt.users, tt.projects)
		}
	}
}

func TestParseFlagsReportsAllProblems(t *testing.T) {
	_, err := parseFlags([]string{"-rpm", "0", "-page-size", "500", "-base-url", "ftp://x",
		"-timeout", "0s", "-users-interval", "1ms"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"-rpm", "-page-size", "-base-url", "-timeout", "-users-interval"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestParseFlagsLogging(t *testing.T) {
	if _, err := parseFlags([]string{"-log-format", "json", "-log-level", "debug"}); err != nil {
		t.Errorf("valid logging flags rejected: %v", err)
	}
	_, err := parseFlags([]string{"-log-format", "xml", "-log-level", "loud"})
	if err == nil || !strings.Contains(err.Error(), "-log-format") || !strings.Contains(err.Error(), "-log-level") {
		t.Errorf("err = %v, want both logging flags reported", err)
	}
}

func TestRateBudgetWarnsWhenOverBudget(t *testing.T) {
	var buf bytes.Buffer
	b := newRateBudget(150, slog.New(slog.NewTextHandler(&buf, nil)))

	b.record("projects", 3, 5*time.Minute) // 0.6/min
	if strings.Contains(buf.String(), "exceeds") {
		t.Fatalf("unexpected warning: %s", buf.String())
	}
	b.record("users", 101, 30*time.Second) // 202/min > 0.7*150
	if !strings.Contains(buf.String(), "exceeds the rate budget") {
		t.Errorf("expected a budget warning, got: %s", buf.String())
	}
}
