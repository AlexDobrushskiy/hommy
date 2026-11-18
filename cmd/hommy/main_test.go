package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := run([]string{"version"}, &out, &errb); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, errb.String())
	}
	if !strings.HasPrefix(out.String(), "hommy ") {
		t.Fatalf("got %q", out.String())
	}
}

func TestUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := run(nil, &out, &errb); rc != 2 || !strings.Contains(errb.String(), "usage") {
		t.Fatalf("rc=%d stderr=%q", rc, errb.String())
	}
}
