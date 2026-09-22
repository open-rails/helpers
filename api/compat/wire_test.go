package compat

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden fixtures")

// capture drives handler over a real socket and renders the response the way a
// client receives it.
func capture(t *testing.T, handler http.Handler, reqHeaders map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range reqHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %s\n", resp.Status)
	names := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		// Date and Content-Length vary per run; they are not part of the contract.
		if k == "Date" || k == "Content-Length" {
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(resp.Header[k], ", "))
	}
	b.WriteString("\n")
	b.Write(body)
	return b.String()
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("golden", name+".http")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing fixture %s (run: go test ./... -update): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("%s drifted.\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

func netHTTP(fn func(w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(fn)
}
