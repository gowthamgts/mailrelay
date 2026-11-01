package main

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/gowthamgts/mailrelay/internal/httpcsrf"
)

func TestRulesHTTPImport(t *testing.T) {
	h := newUIHarness(t)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Get(h.url("/rules"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var token string
	for _, c := range jar.Cookies(resp.Request.URL) {
		if c.Name == httpcsrf.CookieName {
			token = c.Value
		}
	}
	data := `version: 1
rules:
  - name: http rule
    enabled: true
    position: 0
    match: {}
    webhook:
      url: https://example.com/hook
      method: POST
`
	post := func(query, body, csrf string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", h.url("/rules/import?"+query), strings.NewReader(body))
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := post("mode=merge", data, ""); code != 403 {
		t.Fatalf("missing csrf: %d", code)
	}
	if code, body := post("mode=merge", data, token); code != 200 || !strings.Contains(body, "created") {
		t.Fatalf("preview: %d %s", code, body)
	}
	records, _ := h.store.ListRules(context.Background())
	if len(records) != 0 {
		t.Fatal("preview mutated rules")
	}
	if code, body := post("mode=merge&apply=true", data, token); code != 200 {
		t.Fatalf("apply: %d %s", code, body)
	}
	if code, body := post("mode=merge", data, token); code != 200 || !strings.Contains(body, "unchanged") {
		t.Fatalf("unchanged preview: %d %s", code, body)
	}
	changed := strings.Replace(data, "enabled: true", "enabled: false", 1)
	if code, body := post("mode=merge", changed, token); code != 200 || !strings.Contains(body, `"fields":["enabled"]`) {
		t.Fatalf("updated preview: %d %s", code, body)
	}
	if code, body := post("mode=replace", "version: 1\nrules: []", token); code != 200 || !strings.Contains(body, "deleted") {
		t.Fatalf("delete preview: %d %s", code, body)
	}
	if code, _ := post("mode=replace&apply=true", strings.Replace(data, "https://example.com/hook", "not-a-url", 1), token); code != 400 {
		t.Fatalf("invalid import: %d", code)
	}
	records, _ = h.store.ListRules(context.Background())
	if len(records) != 1 {
		t.Fatal("invalid replace changed database")
	}
	for _, format := range []string{"yaml", "json"} {
		resp, err := client.Get(h.url("/rules/export?format=" + format))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "rules."+format) || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("bad export response")
		}
	}
}
