//go:build mmxprofile

package main

// Performance builds only (-tags mmxprofile): MMX_PPROF=address serves
// net/http/pprof so tests/performance can profile a running controller.

import (
	"net/http"
	_ "net/http/pprof"
	"os"
)

func init() {
	if address := os.Getenv("MMX_PPROF"); address != "" {
		go func() { _ = http.ListenAndServe(address, nil) }()
	}
}
