package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/korjavin/zulip-gcal-service/internal/store"
)

func TestHealthz(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	h := routes(st)
	get := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		return rec.Code
	}
	if c := get(); c != http.StatusOK {
		t.Fatalf("healthz = %d", c)
	}
	st.Close()
	if c := get(); c != http.StatusServiceUnavailable {
		t.Fatalf("healthz on closed db = %d", c)
	}
}
