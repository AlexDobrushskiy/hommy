// Package envfile reads settings from the process environment.
package envfile

import "os"

// Get returns the variable's value, or def when it is unset or empty.
func Get(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
