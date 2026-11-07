package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/inspecto-system/report-service/internal/config"
)

// requestTimeout bounds one conversion; reports with many photos take a few seconds.
const requestTimeout = 90 * time.Second

var (
	// ErrUnavailable means Gotenberg could not convert now; the job is retried.
	ErrUnavailable = errors.New("gotenberg unavailable")
	// ErrRenderFailed means Gotenberg rejected the document; retrying will not help.
	ErrRenderFailed = errors.New("render failed")
	// ErrTooLarge means the PDF exceeds the configured limit (REPORT_TOO_LARGE).
	ErrTooLarge = errors.New("report too large")
)

// footer numbers the pages; Chromium fills the pageNumber and totalPages classes.
const footer = `<html><head><style>body { font-size: 8pt; color: #5b6475; width: 100%; text-align: center; margin: 0 }</style></head>` +
	`<body><span class="pageNumber"></span> / <span class="totalPages"></span></body></html>`

// Gotenberg converts HTML to PDF with headless Chromium (ADR-012).
type Gotenberg struct {
	base string
	http *http.Client
}

// NewGotenberg returns a client of the Gotenberg API.
func NewGotenberg(cfg config.Gotenberg) *Gotenberg {
	return &Gotenberg{base: strings.TrimRight(cfg.URL, "/"), http: &http.Client{Timeout: requestTimeout}}
}

// PDF converts the HTML of a report; maxBytes bounds the result.
func (g *Gotenberg) PDF(ctx context.Context, html []byte, maxBytes int64) ([]byte, error) {
	body, contentType, err := form(html)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+"/forms/chromium/convert/html", body)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode >= http.StatusInternalServerError {
			return nil, fmt.Errorf("%w: %d %s", ErrUnavailable, resp.StatusCode, detail)
		}
		return nil, fmt.Errorf("%w: %d %s", ErrRenderFailed, resp.StatusCode, detail)
	}
	pdf, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read PDF: %w", ErrUnavailable, err)
	}
	if int64(len(pdf)) > maxBytes {
		return nil, ErrTooLarge
	}
	return pdf, nil
}

// Ping checks that Gotenberg answers, for the readiness probe.
func (g *Gotenberg) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+"/health", nil)
	if err != nil {
		return fmt.Errorf("gotenberg: %w", err)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("gotenberg: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gotenberg: health answered %d", resp.StatusCode)
	}
	return nil
}

// form builds the multipart request: the page, the footer and A4 paper with margins in inches.
func form(html []byte) (io.Reader, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	files := map[string][]byte{"index.html": html, "footer.html": []byte(footer)}
	for _, name := range []string{"index.html", "footer.html"} {
		part, err := w.CreateFormFile("files", name)
		if err != nil {
			return nil, "", fmt.Errorf("build form: %w", err)
		}
		if _, err := part.Write(files[name]); err != nil {
			return nil, "", fmt.Errorf("build form: %w", err)
		}
	}
	fields := [][2]string{
		{"paperWidth", "8.27"}, {"paperHeight", "11.7"},
		{"marginTop", "0.6"}, {"marginBottom", "0.7"}, {"marginLeft", "0.6"}, {"marginRight", "0.6"},
		{"printBackground", "true"},
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return nil, "", fmt.Errorf("build form: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("build form: %w", err)
	}
	return &buf, w.FormDataContentType(), nil
}

// pageObject matches page dictionaries ("/Type /Page"), not the page tree ("/Type /Pages").
var pageObject = regexp.MustCompile(`/Type\s*/Page[^s]`)

// PageCount counts the pages of a PDF written by Chromium, which keeps page dictionaries outside
// compressed object streams; tests compare it with a full PDF parser.
func PageCount(pdf []byte) int {
	return len(pageObject.FindAll(pdf, -1))
}
