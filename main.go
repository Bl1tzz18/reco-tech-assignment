// Command reco-tech-assignment extracts users and projects from the Asana API
// and stores each record as its own JSON file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"reco-tech-assignment/internal/client"
	"reco-tech-assignment/internal/extractor"
	"reco-tech-assignment/internal/storage"
)

const (
	tokenEnv          = "API_TOKEN"
	defaultBaseURL    = "https://app.asana.com/api/1.0"
	defaultRPM        = 150 // Asana free tier, per token; paid plans allow 1500
	defaultMaxRetries = 4
	defaultPageSize   = 100 // Asana's maximum
	maxPageSize       = 100
	defaultTimeout    = 10 * time.Second
	defaultDataDir    = "data"
	defaultEnvFile    = ".env"
)

type config struct {
	baseURL    string
	workspace  string
	dataDir    string
	envFile    string
	rpm        int
	maxRetries int
	pageSize   int
	timeout    time.Duration
	debug      bool
}

func parseFlags(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("reco-tech-assignment", flag.ContinueOnError)
	fs.StringVar(&cfg.baseURL, "base-url", defaultBaseURL, "Asana API base URL")
	fs.StringVar(&cfg.workspace, "workspace", "", "only extract this workspace gid (default: all workspaces)")
	fs.StringVar(&cfg.dataDir, "data-dir", defaultDataDir, "output directory")
	fs.StringVar(&cfg.envFile, "env-file", defaultEnvFile, "optional KEY=VALUE file; real env vars take precedence")
	fs.IntVar(&cfg.rpm, "rpm", defaultRPM, "client-side rate limit, requests per minute")
	fs.IntVar(&cfg.maxRetries, "max-retries", defaultMaxRetries, "retries for 408/429/5xx/network errors")
	fs.IntVar(&cfg.pageSize, "page-size", defaultPageSize, "items per page (1-100)")
	fs.DurationVar(&cfg.timeout, "timeout", defaultTimeout, "per-request HTTP timeout")
	fs.BoolVar(&cfg.debug, "debug", false, "debug logging (logs every page)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	return cfg, cfg.validate()
}

// validate reports every problem at once instead of one per run.
func (c config) validate() error {
	var errs []error
	if u, err := url.Parse(c.baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("-base-url %q must be an absolute http(s) URL", c.baseURL))
	}
	if c.rpm <= 0 {
		errs = append(errs, fmt.Errorf("-rpm must be > 0, got %d", c.rpm))
	}
	if c.maxRetries < 0 {
		errs = append(errs, fmt.Errorf("-max-retries must be >= 0, got %d", c.maxRetries))
	}
	if c.pageSize < 1 || c.pageSize > maxPageSize {
		errs = append(errs, fmt.Errorf("-page-size must be 1..%d, got %d", maxPageSize, c.pageSize))
	}
	if c.timeout <= 0 {
		errs = append(errs, fmt.Errorf("-timeout must be > 0, got %s", c.timeout))
	}
	if c.dataDir == "" {
		errs = append(errs, errors.New("-data-dir must not be empty"))
	}
	return errors.Join(errs...)
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	level := slog.LevelInfo
	if cfg.debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := run(cfg, log); err != nil {
		log.Error("extraction failed", "err", err)
		os.Exit(1)
	}
}

func run(cfg config, log *slog.Logger) error {
	if err := loadDotEnv(cfg.envFile, log); err != nil {
		return fmt.Errorf("load env file: %w", err)
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		return fmt.Errorf("%s is not set (export it or put it in %s)", tokenEnv, cfg.envFile)
	}
	// Ctrl+C / SIGTERM cancel the context, which aborts in-flight requests and sleeps.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	api, err := client.New(cfg.baseURL, token, client.Options{
		RequestsPerMinute: cfg.rpm,
		MaxRetries:        cfg.maxRetries,
		PageSize:          cfg.pageSize,
		Timeout:           cfg.timeout,
		Logger:            log,
	})
	if err != nil {
		return fmt.Errorf("create API client: %w", err)
	}
	store, err := storage.New(cfg.dataDir)
	if err != nil {
		return err
	}
	ex := extractor.New(api, store, cfg.workspace, log)

	// Never log the token.
	log.Info("starting extraction", "base_url", cfg.baseURL, "workspace", cfg.workspace,
		"data_dir", cfg.dataDir, "rpm", cfg.rpm, "page_size", cfg.pageSize)
	start := time.Now()
	// Run both even if one fails: users and projects are independent.
	err = errors.Join(ex.SyncUsers(ctx), ex.SyncProjects(ctx))
	if err != nil {
		return err
	}
	log.Info("extraction complete", "took", time.Since(start).Round(time.Millisecond))
	return nil
}
