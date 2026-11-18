package auth

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestResultPage_SuccessAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		note   string
	}{
		{"success", http.StatusOK, `<p class="note success" role="status">`},
		{"failure", http.StatusBadRequest, `<p class="note error" role="alert">`},
	} {
		rec := httptest.NewRecorder()
		writeResultPage(rec, tc.status, "You're signed in", "You can close this tab and return to your terminal.")
		body := rec.Body.String()
		if rec.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.status)
		}
		if !strings.Contains(body, tc.note) {
			t.Errorf("%s: the page has no %s", tc.name, tc.note)
		}
		if !strings.Contains(body, "<h1>You&#39;re signed in</h1>") || !strings.Contains(body, "You can close this tab and return to your terminal.") {
			t.Errorf("%s: the heading or the detail is missing:\n%s", tc.name, body)
		}
		for header, want := range map[string]string{
			"Content-Type":            "text/html; charset=utf-8",
			"Content-Security-Policy": resultPageCSP,
			"Cache-Control":           "no-store",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", tc.name, header, got, want)
			}
		}
	}
}

// TestResultPage_LoadsNothing proves the page draws offline: no URL but the inline font, which is the embedded file.
func TestResultPage_LoadsNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	writeResultPage(rec, http.StatusOK, "You're signed in", "You can close this tab and return to your terminal.")
	body := rec.Body.String()
	for _, ref := range []string{"src=", "href=", "http://", "https://", "@import"} {
		if strings.Contains(body, ref) {
			t.Errorf("the page refers to something outside it: %q", ref)
		}
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Fatal("html/template filtered a value out of the page (ZgotmplZ): the @font-face rule must stay template.CSS")
	}
	urls := regexp.MustCompile(`url\(([^)]*)\)`).FindAllStringSubmatch(body, -1)
	if len(urls) != 1 || !strings.HasPrefix(urls[0][1], "data:font/woff2;base64,") {
		t.Fatalf("want exactly one url(), the inline font; got %d", len(urls))
	}
	if len(outfitLatin) == 0 {
		t.Fatal("the font is not embedded")
	}
}

func TestResultPage_EscapesItsText(t *testing.T) {
	rec := httptest.NewRecorder()
	writeResultPage(rec, http.StatusBadRequest, "<script>x</script>", `"><img onerror=x>`)
	body := rec.Body.String()
	if strings.Contains(body, "<script>x") || strings.Contains(body, "<img") {
		t.Errorf("the heading or detail went into the page unescaped:\n%s", body)
	}
}
