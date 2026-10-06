package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

// healthcheck GETs url and returns a process exit code: 0 when it answers 200.
// It exists because the distroless image has no shell or curl; compose runs
// `zulip-gcal-service healthcheck` instead.
func healthcheck(url string) int {
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}
