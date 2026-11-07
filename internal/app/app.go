// Package app wires the service together and runs it until the context is cancelled.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/inspecto-system/report-service/internal/auth"
	"github.com/inspecto-system/report-service/internal/config"
	"github.com/inspecto-system/report-service/internal/core"
	"github.com/inspecto-system/report-service/internal/httpapi"
	"github.com/inspecto-system/report-service/internal/jobs"
	"github.com/inspecto-system/report-service/internal/media"
	"github.com/inspecto-system/report-service/internal/objectstore"
	"github.com/inspecto-system/report-service/internal/render"
	"github.com/inspecto-system/report-service/internal/service"
	"github.com/inspecto-system/report-service/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Timeouts of the HTTP server; they limit slow clients (SECURITY.md §4, resource consumption).
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
	shutdownTimeout   = 20 * time.Second
)

// Run migrates the database, then serves HTTP on the listener until ctx is cancelled and the
// in-flight requests have finished.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, listener net.Listener) error {
	pool, err := prepareDatabase(ctx, cfg.Database)
	if err != nil {
		// Nothing was served yet; release the port for the caller.
		_ = listener.Close()
		return err
	}
	defer pool.Close()
	verifier, err := auth.NewVerifier(ctx, cfg.Auth)
	if err != nil {
		_ = listener.Close()
		return err
	}
	zone, err := time.LoadLocation(cfg.Reports.TimeZone)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("load time zone: %w", err)
	}

	server := &http.Server{
		Handler:           wire(ctx, cfg, logger, resources{pool: pool, verifier: verifier, zone: zone}),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	logger.InfoContext(ctx, "report-service started", slog.String("address", listener.Addr().String()))

	select {
	case err := <-served:
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down HTTP server: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", err)
	}
	logger.InfoContext(ctx, "report-service stopped")
	return nil
}

// resources are prepared by Run before anything is served.
type resources struct {
	pool     *pgxpool.Pool
	verifier *auth.Verifier
	zone     *time.Location
}

// wire builds the services and the HTTP handler and starts the report workers, which stop with ctx.
func wire(ctx context.Context, cfg config.Config, logger *slog.Logger, res resources) http.Handler {
	repo := storage.NewReports(res.pool)
	files := objectstore.New(cfg.Storage)
	coreAPI := core.New(cfg.Core)
	gotenberg := render.NewGotenberg(cfg.Gotenberg)
	sources := jobs.Sources{
		ReportData: coreAPI.ReportData,
		Photo:      media.New(cfg.Media, coreAPI).Preview,
		PDF:        gotenberg.PDF,
		Store:      files.Put,
	}
	go jobs.NewGenerator(repo, sources, jobs.Settings{
		Workers:       cfg.Reports.Workers,
		MaxAttempts:   cfg.Reports.MaxAttempts,
		MaxSizeBytes:  cfg.Reports.MaxSizeBytes,
		RenderTimeout: cfg.Reports.RenderTimeout,
		Zone:          res.zone,
	}, logger).Run(ctx)
	return httpapi.NewRouter(httpapi.Dependencies{
		Logger:   logger,
		Ready:    []httpapi.Pinger{res.pool, files, gotenberg},
		Verifier: res.verifier,
		Services: res.verifier,
		Reports:  service.NewReports(repo, files, coreAPI),
	})
}

// prepareDatabase applies migrations as the schema owner and connects as the DML-only role.
func prepareDatabase(ctx context.Context, db config.Database) (*pgxpool.Pool, error) {
	migratorURL, err := db.URLFor(db.Migrator)
	if err != nil {
		return nil, err
	}
	if err := storage.Migrate(migratorURL); err != nil {
		return nil, err
	}
	appURL, err := db.URLFor(db.App)
	if err != nil {
		return nil, err
	}
	return storage.Connect(ctx, appURL)
}
