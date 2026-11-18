// Package staticsite serves a directory of fixture pages (e2e/fixtures/static/) for browser rows.
package staticsite

import (
	"fmt"
	"net/http"
	"os"
)

// Handler serves dir; it refuses a missing directory so a wrong path fails at start, not on the first request.
func Handler(dir string) (http.Handler, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	return http.FileServer(http.Dir(dir)), nil
}
