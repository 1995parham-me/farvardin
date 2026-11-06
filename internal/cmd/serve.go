package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/auth"
	"github.com/1995parham-me/farvardin/internal/infra/config"
	"github.com/1995parham-me/farvardin/internal/infra/dav"
	"github.com/1995parham-me/farvardin/internal/infra/metrics"
	"github.com/1995parham-me/farvardin/internal/infra/push"
	"github.com/1995parham-me/farvardin/internal/infra/store"
	"github.com/1995parham-me/farvardin/internal/infra/web"
)

func serveCommand() *cli.Command {
	//nolint:exhaustruct // command literal
	return &cli.Command{
		Name:  "serve",
		Usage: "run the CardDAV/CalDAV server and the web UI",
		Flags: []cli.Flag{
			//nolint:exhaustruct // flag literal
			&cli.StringFlag{Name: "listen", Aliases: []string{"l"}, Usage: "address to listen on (overrides server.listen)"},
			//nolint:exhaustruct // flag literal
			&cli.StringFlag{Name: "root", Usage: "store root directory (overrides store.root)"},
		},
		Action: serve,
	}
}

func newLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Level))

	opts := &slog.HandlerOptions{Level: level, AddSource: false, ReplaceAttr: nil}
	if strings.EqualFold(cfg.Format, "json") {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}

	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

const uiPath = "/ui"

func serve(ctx context.Context, cmd *cli.Command) error {
	cfg, err := config.Load(cmd.String("config"))
	if err != nil {
		return err
	}

	if cmd.IsSet("listen") {
		cfg.Server.Listen = cmd.String("listen")
	}

	if cmd.IsSet("root") {
		cfg.Store.Root = cmd.String("root")
	}

	log := newLogger(cfg.Log)

	s, err := store.Open(cfg.Store.Root, cfg.Store.Principal, store.Author{Name: cfg.Store.AuthorName, Email: cfg.Store.AuthorEmail})
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}

	if cfg.Store.Init {
		if err := s.EnsureDefaults(); err != nil {
			return fmt.Errorf("creating default collections: %w", err)
		}
	}

	var m *metrics.Metrics
	if cfg.Server.Metrics {
		m = metrics.New()
	}

	davUI := ""
	if cfg.Server.UI {
		davUI = uiPath + "/"
	}

	pushSvc, err := newPush(cfg.Push, log)
	if err != nil {
		return err
	}

	davHandler := dav.New(s, davOptions(cfg, davUI, pushSvc), log, m)

	mux := http.NewServeMux()
	guard := auth.New(cfg.Auth.Mode, cfg.Auth.Realm, cfg.Auth.Header, cfg.Auth.Users)

	if cfg.Server.UI {
		ui, err := web.New(s, davHandler.Cache(), davHandler.Birthdays, uiPath, cfg.Server.Prefix, log)
		if err != nil {
			return err
		}

		mux.Handle(uiPath+"/", guard.Wrap(ui))
	}

	if m != nil && cfg.Server.MetricsListen == "" {
		mux.Handle("GET /metrics", m.Handler())
	}

	if m != nil && cfg.Server.MetricsListen != "" {
		go serveMetrics(cfg.Server.MetricsListen, m, log)
	}

	mux.Handle("/", guard.Wrap(davHandler))

	//nolint:exhaustruct // only the timeouts that matter are set
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           logRequests(log, m, mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)

	go func() {
		log.Info("listening", "address", cfg.Server.Listen, "root", cfg.Store.Root, "principal", cfg.Store.Principal,
			"addressbooks", len(s.Collections(store.AddressBook)), "calendars", len(s.Collections(store.Calendar)))
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()

		log.Info("shutting down")

		return srv.Shutdown(shutdown) //nolint:wrapcheck // shutdown errors are reported as they are
	}
}

func davOptions(cfg config.Config, uiPath string, pushSvc *push.Service) dav.Options {
	return dav.Options{
		Prefix:          cfg.Server.Prefix,
		UIPath:          uiPath,
		MaxResourceSize: cfg.Contacts.MaxResourceSize,
		Photo: contact.PhotoPolicy{
			MaxSize: cfg.Contacts.PhotoMaxSize, Guard: cfg.Contacts.PhotoGuard, Stabilize: cfg.Contacts.PhotoStabilize,
			ThumbnailSize: cfg.Contacts.ThumbnailSize, Quality: cfg.Contacts.PhotoQuality,
		},
		Birthdays:             cfg.Calendars.Birthdays,
		BirthdayAlarmDays:     cfg.Calendars.BirthdayAlarmDays,
		BirthdayColor:         cfg.Calendars.BirthdayColor,
		AllowCollectionDelete: false,
		CalendarUserAddress:   cfg.Calendars.UserAddress,
		Push:                  pushSvc,
	}
}

func newPush(cfg config.Push, log *slog.Logger) (*push.Service, error) {
	if !cfg.Enabled {
		return nil, nil //nolint:nilnil // nil means push is off
	}

	keys, err := push.LoadKeys(filepath.Join(cfg.StateDir, "vapid", "private.pem"), cfg.Subject)
	if err != nil {
		return nil, fmt.Errorf("loading the VAPID key: %w", err)
	}

	log.Info("WebDAV-Push enabled", "state", cfg.StateDir, "vapid", keys.Public)

	return push.New(keys, cfg.StateDir, log), nil
}

func serveMetrics(addr string, m *metrics.Metrics, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())

	//nolint:exhaustruct // only the timeouts that matter are set
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("metrics listener stopped", "address", addr, "error", err)
	}
}

type statusRecorder struct {
	http.ResponseWriter

	status int
}

// WriteHeader records the status code for logs and metrics.
func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func logRequests(log *slog.Logger, m *metrics.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		d := time.Since(start)
		m.Request(r.Method, rec.status, d)
		log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", d, "agent", r.UserAgent())
	})
}
