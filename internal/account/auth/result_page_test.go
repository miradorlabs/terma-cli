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
		name    string
		write   func(http.ResponseWriter)
		status  int
		note    string
		heading string
		detail  string
	}{
		{
			"signed in", writeSignedIn, http.StatusOK, `<p class="note success" role="status">`,
			"<h1>You&#39;re signed in</h1>", "You can close this tab and return to your terminal.",
		},
		{
			"failed", func(w http.ResponseWriter) { writeAuthFailed(w, "No authorization code was returned.") },
			http.StatusBadRequest, `<p class="note error" role="alert">`, "<h1>Authorization failed</h1>", "No authorization code was returned.",
		},
	} {
		rec := httptest.NewRecorder()
		tc.write(rec)
		body := rec.Body.String()
		if rec.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.status)
		}
		for _, want := range []string{tc.note, tc.heading, "<span>" + tc.detail + "</span>"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the page has no %s", tc.name, want)
			}
		}
		if strings.Contains(body, "{{") {
			t.Errorf("%s: a placeholder was left unfilled", tc.name)
		}
		// Both icons are in the page; this rule shows the one that matches the note's tone.
		if !strings.Contains(body, ".success .icon-error, .error .icon-success { display: none; }") {
			t.Errorf("%s: nothing hides the other outcome's icon", tc.name)
		}
		for header, want := range map[string]string{
			"Content-Type":            "text/html; charset=utf-8",
			"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; font-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
			"Cache-Control":           "no-store",
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
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
	writeSignedIn(rec)
	body := rec.Body.String()
	for _, ref := range []string{"src=", "href=", "http://", "https://", "@import"} {
		if strings.Contains(body, ref) {
			t.Errorf("the page refers to something outside it: %q", ref)
		}
	}
	urls := regexp.MustCompile(`url\(([^)]*)\)`).FindAllStringSubmatch(body, -1)
	if len(urls) != 1 || !strings.HasPrefix(urls[0][1], "data:font/woff2;base64,") {
		t.Fatalf("want exactly one url(), the inline font; got %d", len(urls))
	}
	if len(outfitLatin) == 0 {
		t.Fatal("the font is not embedded")
	}
}

// TestResultPage_EscapesItsText covers markup and a placeholder in the detail: one is escaped, the other left as text.
func TestResultPage_EscapesItsText(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAuthFailed(rec, `<script>x</script>"><img onerror=x> {{font-face}}`)
	body := rec.Body.String()
	if strings.Contains(body, "<script>x") || strings.Contains(body, "<img") {
		t.Errorf("the detail went into the page unescaped:\n%s", body)
	}
	if strings.Count(body, "data:font/woff2") != 1 || !strings.Contains(body, "{{font-face}}</span>") {
		t.Error("a placeholder in the detail was filled: a page fills only its own placeholders")
	}
}
