// Package media reads photo bytes from the internal API of media-service for the PDF (ADR-026).
package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/config"
)

const (
	requestTimeout = 20 * time.Second
	// maxPhotoBytes bounds one preview; media-service keeps previews at about 1280 px.
	maxPhotoBytes = 8 << 20
)

// ErrUnavailable means media-service could not give the photo now; the job is retried.
var ErrUnavailable = errors.New("media-service unavailable")

// ErrNotFound means the photo no longer exists (for example, it was detached meanwhile).
var ErrNotFound = errors.New("photo not found")

// Tokens supplies service tokens with scope photos.read (issued by core-service).
type Tokens interface {
	ServiceToken(ctx context.Context) (string, error)
}

// Client is safe for concurrent use.
type Client struct {
	base   string
	tokens Tokens
	http   *http.Client
}

// New returns a client of the media-service internal API.
func New(cfg config.Media, tokens Tokens) *Client {
	return &Client{base: strings.TrimRight(cfg.URL, "/"), tokens: tokens, http: &http.Client{Timeout: requestTimeout}}
}

// Preview returns the JPEG bytes of the preview variant of the photo.
func (c *Client) Preview(ctx context.Context, photoID uuid.UUID) ([]byte, error) {
	token, err := c.tokens.ServiceToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	endpoint := c.base + "/internal/v1/photos/" + photoID.String() + "/content?variant=preview"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("%w: photo answered %d", ErrUnavailable, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPhotoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read photo: %w", ErrUnavailable, err)
	}
	if len(data) > maxPhotoBytes {
		return nil, fmt.Errorf("%w: photo larger than %d bytes", ErrUnavailable, maxPhotoBytes)
	}
	return data, nil
}
