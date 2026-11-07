package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/inspecto-system/report-service/internal/config"
	"github.com/inspecto-system/report-service/internal/testsupport"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return listener
}

func send(t *testing.T, req *http.Request) (int, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

func newRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestRunServesHealthAndStopsGracefully(t *testing.T) {
	cfg := config.Config{Database: testsupport.StartPostgres(t).Config}
	listener := listen(t)
	base := "http://" + listener.Addr().String()
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- Run(ctx, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), listener) }()

	waitReady(t, base, &logs)

	if status, _ := send(t, newRequest(t, http.MethodGet, base+"/health/live", nil)); status != http.StatusOK {
		t.Errorf("live: %d", status)
	}
	status, body := send(t, newRequest(t, http.MethodGet, base+"/api/v1/unknown", nil))
	if status != http.StatusNotFound || !strings.Contains(string(body), "NOT_FOUND") {
		t.Errorf("unknown path: %d %s", status, body)
	}

	cancel()
	waitStopped(t, done, &logs)
}

func waitReady(t *testing.T, base string, logs *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if status, _ := send(t, newRequest(t, http.MethodGet, base+"/health/ready", nil)); status == http.StatusOK {
			return
		}
	}
	t.Fatalf("not ready; logs: %s", logs.String())
}

func waitStopped(t *testing.T, done <-chan error, logs *bytes.Buffer) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if !strings.Contains(logs.String(), "report-service stopped") {
		t.Errorf("no stop record in logs: %s", logs.String())
	}
}

func TestRunFailsWithoutDatabaseAndReleasesListener(t *testing.T) {
	listener := listen(t)
	cfg := config.Config{Database: config.Database{
		URL:      "postgres://127.0.0.1:1/inspecto_report?sslmode=disable&connect_timeout=1",
		App:      config.Credentials{User: "report_app", Password: "x"},
		Migrator: config.Credentials{User: "report_migrator", Password: "x"},
	}}

	err := Run(context.Background(), cfg, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), listener)

	if err == nil || !strings.Contains(err.Error(), "migrations") {
		t.Fatalf("err = %v", err)
	}
	if _, acceptErr := listener.Accept(); !errors.Is(acceptErr, net.ErrClosed) {
		t.Errorf("listener still open: %v", acceptErr)
	}
}
