package config

import (
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func required() map[string]string {
	return map[string]string{
		"REPORT_DB_APP_PASSWORD":      "app-secret",
		"REPORT_DB_MIGRATOR_PASSWORD": "migrator-secret",
		"REPORT_CORE_CLIENT_SECRET":   "client-secret",
		"REPORT_S3_ACCESS_KEY":        "report",
		"REPORT_S3_SECRET_KEY":        "s3-secret",
	}
}

func TestLoadUsesDefaults(t *testing.T) {
	cfg, err := Load(env(required()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := Config{
		Port: 8083,
		Database: Database{
			URL:      "postgres://127.0.0.1:5432/inspecto_report?sslmode=require",
			App:      Credentials{User: "report_app", Password: "app-secret"},
			Migrator: Credentials{User: "report_migrator", Password: "migrator-secret"},
		},
		Auth: Auth{
			JWKSURL: "http://127.0.0.1:8081/api/v1/.well-known/jwks.json", Issuer: "inspecto-core", Audience: "inspecto-api",
			InternalAudience: "inspecto-internal",
		},
		Core:      Core{URL: "http://127.0.0.1:8081", ClientID: "report-service", ClientSecret: "client-secret"},
		Media:     Media{URL: "http://127.0.0.1:8082"},
		Gotenberg: Gotenberg{URL: "http://127.0.0.1:3000"},
		Storage: Storage{
			Endpoint: "http://127.0.0.1:8333", Region: "us-east-1", Bucket: "reports", AccessKey: "report", SecretKey: "s3-secret",
		},
		Reports: Reports{
			MaxSizeBytes: 20 << 20, Workers: 2, MaxAttempts: 3, RenderTimeout: 2 * time.Minute, TimeZone: "Europe/Moscow",
		},
	}
	if cfg != want {
		t.Errorf("Load = %+v, want %+v", cfg, want)
	}
}

func TestLoadReadsOverrides(t *testing.T) {
	values := required()
	values["REPORT_PORT"] = "9090"
	values["REPORT_DB_URL"] = "postgresql://db:5432/report?sslmode=verify-full"
	values["REPORT_JWT_ISSUER"] = "core"

	cfg, err := Load(env(values))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9090 || cfg.Database.URL != "postgresql://db:5432/report?sslmode=verify-full" || cfg.Auth.Issuer != "core" {
		t.Errorf("unexpected config %+v", cfg)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]struct {
		key, value, message string
	}{
		"port not a number":    {"REPORT_PORT", "http", "REPORT_PORT"},
		"port out of range":    {"REPORT_PORT", "70000", "REPORT_PORT"},
		"not postgres":         {"REPORT_DB_URL", "mysql://db/report", "REPORT_DB_URL"},
		"no host":              {"REPORT_DB_URL", "postgres:///report", "REPORT_DB_URL"},
		"credentials in url":   {"REPORT_DB_URL", "postgres://u:p@db/report", "credentials"},
		"jwks not http":        {"REPORT_JWKS_URL", "file:///etc/passwd", "REPORT_JWKS_URL"},
		"no app password":      {"REPORT_DB_APP_PASSWORD", "", "REPORT_DB_APP_PASSWORD is required"},
		"no migrator password": {"REPORT_DB_MIGRATOR_PASSWORD", "", "REPORT_DB_MIGRATOR_PASSWORD is required"},
		"no client secret":     {"REPORT_CORE_CLIENT_SECRET", "", "REPORT_CORE_CLIENT_SECRET is required"},
		"gotenberg not http":   {"REPORT_GOTENBERG_URL", "ftp://x", "REPORT_GOTENBERG_URL"},
		"size too small":       {"REPORT_MAX_SIZE_BYTES", "10", "REPORT_MAX_SIZE_BYTES"},
		"bad timeout":          {"REPORT_RENDER_TIMEOUT", "soon", "REPORT_RENDER_TIMEOUT"},
		"negative timeout":     {"REPORT_RENDER_TIMEOUT", "-1m", "REPORT_RENDER_TIMEOUT"},
		"unknown time zone":    {"REPORT_TIME_ZONE", "Mars/Olympus", "REPORT_TIME_ZONE"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			values := required()
			values[tc.key] = tc.value
			_, err := Load(env(values))
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.message)
			}
		})
	}
}

func TestLoadReportsAllMissingSecrets(t *testing.T) {
	_, err := Load(env(map[string]string{}))
	if err == nil {
		t.Fatal("want an error")
	}
	for key := range required() {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s not reported in %v", key, err)
		}
	}
}

func TestURLForAddsEscapedCredentials(t *testing.T) {
	db := Database{URL: "postgres://127.0.0.1:55432/inspecto_report?sslmode=require"}

	got, err := db.URLFor(Credentials{User: "report_app", Password: "p@ss/word"})
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}

	want := "postgres://report_app:p%40ss%2Fword@127.0.0.1:55432/inspecto_report?sslmode=require"
	if got != want {
		t.Errorf("URLFor = %q, want %q", got, want)
	}
}

func TestURLForRejectsMalformedURL(t *testing.T) {
	if _, err := (Database{URL: "postgres://db:port/x"}).URLFor(Credentials{}); err == nil {
		t.Error("want an error")
	}
}

func TestCredentialsHidePassword(t *testing.T) {
	c := Credentials{User: "report_app", Password: "secret"}
	if s := c.String(); strings.Contains(s, "secret") {
		t.Errorf("String() = %q exposes the password", s)
	}
}
