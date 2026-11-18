// Package envfile loads KEY=VALUE files into the process environment.
package envfile

import (
	"bufio"
	"os"
	"strings"
)

// Load loads KEY=VALUE lines into the environment without overriding
// variables that are already set. A missing file is not an error.
func Load(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if i := strings.Index(v, " #"); i >= 0 { // strip inline comment
			v = v[:i]
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}

// Get returns the variable's value, or def when it is unset or empty.
func Get(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
