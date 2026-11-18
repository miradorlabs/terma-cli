package auth

import (
	_ "embed"
	"encoding/base64"
	"html"
	"io"
	"net/http"
	"strings"
	"sync"
)

// The page the browser shows once the sign-in callback reaches the listener: Terma's mark, a heading, and a green note
// or a red one, styled like the web app's approval page (terma-frontend's tokens and Outfit, light or dark as the
// system is). It loads nothing, the font included, and its Content-Security-Policy lets it load nothing. It is filled
// with strings.Replacer and html.EscapeString, never a template engine (internal/boundary says why).

//go:embed resultpage/result.html
var resultPageHTML string

// Outfit's Latin subset, as terma-frontend serves it (SIL Open Font License, resultpage/OFL.txt).
//
//go:embed resultpage/outfit-latin-wght-normal.woff2
var outfitLatin []byte

// outfitFontFace is the @font-face rule with the font inline. It is built for the first page shown, so no other
// terma process (a hook, say) pays for encoding it.
var outfitFontFace = sync.OnceValue(func() string {
	return `@font-face {
  font-family: "Outfit Variable";
  font-style: normal;
  font-display: swap;
  font-weight: 100 900;
  src: url(data:font/woff2;base64,` + base64.StdEncoding.EncodeToString(outfitLatin) + `) format("woff2-variations");
  unicode-range: U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2191,U+2193,U+2212,U+2215,U+FEFF,U+FFFD;
}`
})

const resultPageCSP = "default-src 'none'; style-src 'unsafe-inline'; font-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// outcome is how a callback ended, as the page shows it: its status, the note's tone (a CSS class) and ARIA role, and
// the heading.
type outcome struct {
	status              int
	tone, role, heading string
}

var (
	signedIn   = outcome{http.StatusOK, "success", "status", "You're signed in"}
	authFailed = outcome{http.StatusBadRequest, "error", "alert", "Authorization failed"}
)

// writeSignedIn answers the callback that carried this login's code.
func writeSignedIn(w http.ResponseWriter) {
	writeResultPage(w, signedIn, "You can close this tab and return to your terminal.")
}

// writeAuthFailed answers a callback the login cannot use, saying why.
func writeAuthFailed(w http.ResponseWriter, detail string) {
	writeResultPage(w, authFailed, detail)
}

// writeResultPage fills the page. A Replacer makes one pass, so text that looks like a placeholder is never filled.
func writeResultPage(w http.ResponseWriter, o outcome, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", resultPageCSP)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The callback URL carries the code and state; nothing on the page links out, and this keeps it so.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(o.status)
	page := strings.NewReplacer(
		"{{font-face}}", outfitFontFace(),
		"{{tone}}", o.tone,
		"{{role}}", o.role,
		"{{heading}}", html.EscapeString(o.heading),
		"{{detail}}", html.EscapeString(detail),
	).Replace(resultPageHTML)
	_, _ = io.WriteString(w, page)
}
