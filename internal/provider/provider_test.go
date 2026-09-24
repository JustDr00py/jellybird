package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"jellybird/internal/ratelimit"
)

func TestDoWithoutServerRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"success":false,"detail":"DATABASE_ERROR"}`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{HTTP: srv.Client(), Limiter: ratelimit.New(6000)}

	ctx := WithoutServerRetries(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(ctx, req)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
	if !strings.Contains(err.Error(), "HTTP 500: ") || !strings.Contains(err.Error(), "DATABASE_ERROR") {
		t.Errorf("err = %v, want status and body excerpt", err)
	}
}
