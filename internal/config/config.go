// Package config reads the report-service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Config is the complete runtime configuration of the service.
type Config struct {
	// Port is the HTTP port the service listens on.
	Port      int
	Database  Database
	Auth      Auth
	Core      Core
	Media     Media
	Storage   Storage
	Gotenberg Gotenberg
	Reports   Reports
}

// Database describes the connection to the inspecto_report database. Runtime queries use the
// DML-only App role; migrations use the Migrator role that owns the schema (SECURITY.md §6).
type Database struct {
	// URL is a postgres:// URL without credentials, for example
	// postgres://127.0.0.1:5432/inspecto_report?sslmode=require.
	URL      string
	App      Credentials
	Migrator Credentials
}

// Credentials of a database role.
type Credentials struct {
	User     string
	Password string
}

// String hides the password, so credentials can never end up in logs by accident.
func (c Credentials) String() string {
	return c.User + ":***"
}

// Auth describes how user access tokens issued by core-service are verified (ADR-007).
type Auth struct {
	JWKSURL string
	Issuer  string
	// Audience of user access tokens.
	Audience string
	// InternalAudience of service tokens, accepted only on /internal/**.
	InternalAudience string
}

// Core is the internal API of core-service: service tokens, access checks and report data.
type Core struct {
	URL          string
	ClientID     string
	ClientSecret string
}

// Media is the internal API of media-service that serves photo bytes (scope photos.read).
type Media struct {
	URL string
}

// Storage is the S3-compatible object storage holding the PDFs (ADR-024).
type Storage struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
}

// Gotenberg converts the HTML of a report to PDF (ADR-012); it runs without internet access.
type Gotenberg struct {
	URL string
}

// Reports holds the limits of report generation (ADR-011, ADR-026).
type Reports struct {
	// MaxSizeBytes is the largest PDF stored; a larger one fails with REPORT_TOO_LARGE.
	MaxSizeBytes int64
	// Workers is the number of reports rendered at the same time.
	Workers int
	// MaxAttempts bounds the attempts of one job.
	MaxAttempts int
	// RenderTimeout bounds one attempt: report data, photos and the conversion.
	RenderTimeout time.Duration
	// TimeZone is the IANA zone of the dates in reports.
	TimeZone string
}

// Load builds the configuration from the given lookup function (os.Getenv in production) and
// reports every invalid or missing variable at once.
func Load(getenv func(string) string) (Config, error) {
	r := reader{getenv: getenv}
	cfg := Config{
		Port: int(r.integer("REPORT_PORT", 8083, 1, 65535)),
		Database: Database{
			URL:      r.databaseURL("REPORT_DB_URL", "postgres://127.0.0.1:5432/inspecto_report?sslmode=require"),
			App:      Credentials{User: r.text("REPORT_DB_APP_USER", "report_app"), Password: r.secret("REPORT_DB_APP_PASSWORD")},
			Migrator: Credentials{User: r.text("REPORT_DB_MIGRATOR_USER", "report_migrator"), Password: r.secret("REPORT_DB_MIGRATOR_PASSWORD")},
		},
		Auth: Auth{
			JWKSURL:          r.httpURL("REPORT_JWKS_URL", "http://127.0.0.1:8081/api/v1/.well-known/jwks.json"),
			Issuer:           r.text("REPORT_JWT_ISSUER", "inspecto-core"),
			Audience:         r.text("REPORT_JWT_AUDIENCE", "inspecto-api"),
			InternalAudience: r.text("REPORT_JWT_INTERNAL_AUDIENCE", "inspecto-internal"),
		},
		Core: Core{
			URL:          r.httpURL("REPORT_CORE_URL", "http://127.0.0.1:8081"),
			ClientID:     "report-service",
			ClientSecret: r.secret("REPORT_CORE_CLIENT_SECRET"),
		},
		Media:     Media{URL: r.httpURL("REPORT_MEDIA_URL", "http://127.0.0.1:8082")},
		Gotenberg: Gotenberg{URL: r.httpURL("REPORT_GOTENBERG_URL", "http://127.0.0.1:3000")},
		Storage: Storage{
			Endpoint:  r.httpURL("REPORT_S3_ENDPOINT", "http://127.0.0.1:8333"),
			Region:    r.text("REPORT_S3_REGION", "us-east-1"),
			Bucket:    r.text("REPORT_S3_BUCKET", "reports"),
			AccessKey: r.secret("REPORT_S3_ACCESS_KEY"),
			SecretKey: r.secret("REPORT_S3_SECRET_KEY"),
		},
		Reports: Reports{
			MaxSizeBytes:  r.integer("REPORT_MAX_SIZE_BYTES", 20<<20, 1<<20, 200<<20),
			Workers:       int(r.integer("REPORT_WORKERS", 2, 1, 16)),
			MaxAttempts:   int(r.integer("REPORT_MAX_ATTEMPTS", 3, 1, 10)),
			RenderTimeout: r.duration("REPORT_RENDER_TIMEOUT", 2*time.Minute),
			TimeZone:      r.timeZone("REPORT_TIME_ZONE", "Europe/Moscow"),
		},
	}
	if err := errors.Join(r.errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// URLFor returns the database URL with the credentials of the given role.
func (d Database) URLFor(role Credentials) (string, error) {
	u, err := url.Parse(d.URL)
	if err != nil {
		return "", fmt.Errorf("parse database URL: %w", err)
	}
	u.User = url.UserPassword(role.User, role.Password)
	return u.String(), nil
}

// reader reads variables and collects the problems found.
type reader struct {
	getenv func(string) string
	errs   []error
}

func (r *reader) text(key, fallback string) string {
	if value := r.getenv(key); value != "" {
		return value
	}
	return fallback
}

func (r *reader) secret(key string) string {
	value := r.getenv(key)
	if value == "" {
		r.errs = append(r.errs, fmt.Errorf("%s is required", key))
	}
	return value
}

func (r *reader) integer(key string, fallback, minimum, maximum int64) int64 {
	raw := r.getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minimum || value > maximum {
		r.errs = append(r.errs, fmt.Errorf("%s must be a number from %d to %d, got %q", key, minimum, maximum, raw))
		return fallback
	}
	return value
}

func (r *reader) databaseURL(key, fallback string) string {
	value := r.text(key, fallback)
	u, err := url.Parse(value)
	switch {
	case err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "":
		r.errs = append(r.errs, fmt.Errorf("%s must be a postgres:// URL", key))
	case u.User != nil:
		r.errs = append(r.errs, fmt.Errorf("%s must not contain credentials; use REPORT_DB_*_USER and _PASSWORD", key))
	}
	return value
}

func (r *reader) httpURL(key, fallback string) string {
	value := r.text(key, fallback)
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		r.errs = append(r.errs, fmt.Errorf("%s must be an http(s) URL", key))
	}
	return value
}

func (r *reader) duration(key string, fallback time.Duration) time.Duration {
	raw := r.getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		r.errs = append(r.errs, fmt.Errorf("%s must be a positive duration such as 2m, got %q", key, raw))
		return fallback
	}
	return value
}

func (r *reader) timeZone(key, fallback string) string {
	value := r.text(key, fallback)
	if _, err := time.LoadLocation(value); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be an IANA time zone such as Europe/Moscow, got %q", key, value))
	}
	return value
}
