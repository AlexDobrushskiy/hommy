package staticsite

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesFixturePage(t *testing.T) {
	h, err := Handler("../../../e2e/fixtures/static")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "hommy static fixture") {
		t.Fatalf("index: %d", resp.StatusCode)
	}
	if _, err := Handler("no/such/dir"); err == nil {
		t.Fatal("a missing dir was accepted")
	}
}
