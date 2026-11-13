// Command reco-tech-assignment extracts users and projects from the Asana API
// and stores each record as its own JSON file, once or periodically.
package main

import (
	"context"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"reco-tech-assignment/internal/client"
	"reco-tech-assignment/internal/extractor"
	"reco-tech-assignment/internal/obs"
	"reco-tech-assignment/internal/poller"
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
	defaultInterval   = 5 * time.Minute
	minInterval       = 5 * time.Second // a typo like -interval 1ms must not burn the rate budget
	shutdownTimeout   = 5 * time.Second
	readHeaderTimeout = 5 * time.Second
	expvarName        = "asana_extractor"
)

type config struct {
	baseURL          string
	workspace        string
	dataDir          string
	envFile          string
	rpm              int
	maxRetries       int
	pageSize         int
	timeout          time.Duration
	interval         time.Duration
	usersInterval    time.Duration // 0 = interval
	projectsInterval time.Duration // 0 = interval
	once             bool
	logFormat        string
	logLevel         string
	metricsAddr      string // empty = no HTTP server
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
	fs.DurationVar(&cfg.interval, "interval", defaultInterval, "extraction interval for all resources (e.g. 5m or 30s)")
	fs.DurationVar(&cfg.usersInterval, "users-interval", 0, "override -interval for users")
	fs.DurationVar(&cfg.projectsInterval, "projects-interval", 0, "override -interval for projects")
	fs.BoolVar(&cfg.once, "once", false, "extract once and exit")
	fs.StringVar(&cfg.logFormat, "log-format", "text", "log format: text or json")
	fs.StringVar(&cfg.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	fs.StringVar(&cfg.metricsAddr, "metrics-addr", "", "serve /healthz, /metrics, /debug/vars, /debug/pprof on this address (e.g. 127.0.0.1:9090)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.usersInterval == 0 {
		cfg.usersInterval = cfg.interval
	}
	if cfg.projectsInterval == 0 {
		cfg.projectsInterval = cfg.interval
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
	if c.logFormat != "text" && c.logFormat != "json" {
		errs = append(errs, fmt.Errorf("-log-format must be text or json, got %q", c.logFormat))
	}
	if _, err := obs.ParseLevel(c.logLevel); err != nil {
		errs = append(errs, fmt.Errorf("-log-level: %w", err))
	}
	if c.dataDir == "" {
		errs = append(errs, errors.New("-data-dir must not be empty"))
	}
	for name, d := range map[string]time.Duration{
		"-interval": c.interval, "-users-interval": c.usersInterval, "-projects-interval": c.projectsInterval,
	} {
		if d < minInterval {
			errs = append(errs, fmt.Errorf("%s must be >= %s, got %s", name, minInterval, d))
		}
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
	log, err := obs.NewLogger(os.Stderr, cfg.logFormat, cfg.logLevel)
	if err != nil { // already validated; kept for safety
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
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

	metrics := obs.NewMetrics()
	expvar.Publish(expvarName, metrics)
	if cfg.metricsAddr != "" {
		stopServer, err := serveObservability(cfg.metricsAddr, metrics, log)
		if err != nil {
			return err
		}
		defer stopServer()
	}
	// Logged last on every exit path, so even a failed or interrupted run leaves a summary.
	defer func() { log.Info("metrics summary", "metrics", metrics) }()

	// ONE client for all jobs = one shared rate limiter, because Asana limits per token.
	api, err := client.New(cfg.baseURL, token, client.Options{
		RequestsPerMinute: cfg.rpm,
		MaxRetries:        cfg.maxRetries,
		PageSize:          cfg.pageSize,
		Timeout:           cfg.timeout,
		Logger:            log,
		Metrics:           metrics,
	})
	if err != nil {
		return fmt.Errorf("create API client: %w", err)
	}
	store, err := storage.New(cfg.dataDir)
	if err != nil {
		return err
	}
	ex := extractor.New(api, store, cfg.workspace, log, metrics)
	budget := newRateBudget(cfg.rpm, log)
	jobs := []poller.Job{
		{Name: "users", Interval: cfg.usersInterval, Sync: budget.track("users", cfg.usersInterval, ex.SyncUsers)},
		{Name: "projects", Interval: cfg.projectsInterval, Sync: budget.track("projects", cfg.projectsInterval, ex.SyncProjects)},
	}

	// Never log the token.
	log.Info("starting", "mode", mode(cfg.once), "version", version(), "base_url", cfg.baseURL,
		"workspace", cfg.workspace, "data_dir", cfg.dataDir, "rpm", cfg.rpm,
		"users_interval", cfg.usersInterval, "projects_interval", cfg.projectsInterval,
		"metrics_addr", cfg.metricsAddr)

	if cfg.once {
		return runOnce(ctx, jobs, log)
	}
	runner := poller.Runner{Log: log, Metrics: metrics}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Go(func() { runner.Run(ctx, j) })
	}
	wg.Wait()
	log.Info("shutdown complete")
	return nil
}

// runOnce runs every job a single time. Users and projects are independent,
// so one failing doesn't skip the other.
func runOnce(ctx context.Context, jobs []poller.Job, log *slog.Logger) error {
	start := time.Now()
	var errs []error
	for _, j := range jobs {
		if err := poller.RunOnce(obs.With(ctx, "job", j.Name), j); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	log.Info("extraction complete", "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// serveObservability starts the health/metrics/pprof server. Listening
// happens synchronously, so a bad address or a port in use fails startup.
func serveObservability(addr string, m *obs.Metrics, log *slog.Logger) (stop func(), err error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on -metrics-addr %s: %w", addr, err)
	}
	srv := &http.Server{Handler: obs.Handler(m), ReadHeaderTimeout: readHeaderTimeout}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("observability server failed", "err", err)
		}
	}()
	log.Info("observability server listening", "addr", ln.Addr().String())
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Warn("observability server shutdown", "err", err)
		}
	}, nil
}

// version is the VCS revision baked in by `go build` (plus "-dirty" for
// uncommitted changes), so logs show exactly which code produced them.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return info.GoVersion
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev + " " + info.GoVersion
}

func mode(once bool) string {
	if once {
		return "once"
	}
	return "periodic"
}
