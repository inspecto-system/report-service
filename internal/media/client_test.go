package media

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/config"
	"github.com/inspecto-system/report-service/internal/testsupport"
)

type tokens struct{ err error }

func (t tokens) ServiceToken(context.Context) (string, error) { return "service-token", t.err }

func TestPreviewIsReadWithTheServiceToken(t *testing.T) {
	photo, oversized := uuid.New(), uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer service-token":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/internal/v1/photos/"+photo.String()+"/content" && r.URL.Query().Get("variant") == "preview":
			_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF})
		case r.URL.Path == "/internal/v1/photos/"+oversized.String()+"/content":
			_, _ = w.Write(make([]byte, maxPhotoBytes+1))
		case r.URL.Path == "/internal/v1/photos/"+uuid.Nil.String()+"/content":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := New(config.Media{URL: server.URL + "/"}, tokens{})
	ctx := context.Background()

	data, err := client.Preview(ctx, photo)
	testsupport.Require(t, err == nil && len(data) == 3, "Preview = %v, %v", data, err)
	_, err = client.Preview(ctx, uuid.New())
	testsupport.Expect(t, errors.Is(err, ErrNotFound), "missing photo: %v", err)

	failing := map[string]struct {
		client *Client
		photo  uuid.UUID
	}{
		"media error": {client, uuid.Nil},
		"oversized":   {client, oversized},
		"no token":    {New(config.Media{URL: server.URL}, tokens{err: errors.New("core down")}), photo},
		"unreachable": {New(config.Media{URL: "http://127.0.0.1:1"}, tokens{}), photo},
	}
	for name, tc := range failing {
		_, err := tc.client.Preview(ctx, tc.photo)
		testsupport.Expect(t, errors.Is(err, ErrUnavailable), "%s: %v", name, err)
	}
}
