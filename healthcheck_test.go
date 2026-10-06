package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()

	if got := healthcheck(srv.URL + "/healthz"); got != 0 {
		t.Fatalf("healthy: exit %d", got)
	}
	code = http.StatusServiceUnavailable
	if got := healthcheck(srv.URL + "/healthz"); got != 1 {
		t.Fatalf("unhealthy: exit %d", got)
	}
	srv.Close()
	if got := healthcheck(srv.URL + "/healthz"); got != 1 {
		t.Fatalf("down: exit %d", got)
	}
}
