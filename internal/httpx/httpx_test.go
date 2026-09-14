package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bancsdan/eta/internal/transit"
)

func TestDoAppliesHeaderQueryAndMapsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth") != "secret" || r.URL.Query().Get("key") != "k" || r.URL.Query().Get("q") != "x" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("\xef\xbb\xbf{\"a\":1}"))
		case "/unauth":
			w.WriteHeader(http.StatusUnauthorized)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/limited":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			http.Error(w, "boom   boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := New("test", srv.Client())
	c.Header.Set("X-Auth", "secret")
	c.Query.Set("key", "k")
	ctx := context.Background()

	var out struct{ A int }
	if err := c.GetJSON(ctx, srv.URL+"/ok?q=x", &out); err != nil || out.A != 1 {
		t.Fatalf("GetJSON: %v %+v", err, out)
	}
	cases := map[string]error{"/unauth": transit.ErrUnauthorized, "/missing": transit.ErrNotFound, "/limited": transit.ErrRateLimited}
	for path, want := range cases {
		if err := c.GetJSON(ctx, srv.URL+path+"?q=x", &out); !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", path, err, want)
		}
	}
	err := c.GetJSON(ctx, srv.URL+"/other?q=x", &out)
	if err == nil || err.Error() != "test /other: HTTP 500: boom boom" {
		t.Errorf("500: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.GetJSON(cctx, srv.URL+"/ok?q=x", &out); err == nil {
		t.Errorf("cancelled context should fail")
	}
}

func TestRedactAndTrim(t *testing.T) {
	c := New("t", nil)
	c.Redact = []string{"s3cr3t k"}
	if got := c.redact("https://x/?key=s3cr3t+k&a=s3cr3t k"); got != "https://x/?key=***&a=***" {
		t.Errorf("redact = %q", got)
	}
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	if got := Trim(long); len([]rune(got)) != 121 {
		t.Errorf("Trim len = %d", len([]rune(got)))
	}
	if Join("https://a/", "/b") != "https://a/b" {
		t.Errorf("Join")
	}
}
