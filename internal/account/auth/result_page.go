package auth

import (
	_ "embed"
	"encoding/base64"
	"html/template"
	"net/http"
)

// The page the browser shows once the sign-in callback reaches the listener: Terma's mark, a heading, and a green note
// or a red one, styled like the web app's approval page (terma-frontend's tokens and Outfit, light or dark as the
// system is). It loads nothing, the font included, and its Content-Security-Policy lets it load nothing.

//go:embed resultpage/result.html
var resultPageHTML string

// Outfit's Latin subset, as terma-frontend serves it (SIL Open Font License, resultpage/OFL.txt).
//
//go:embed resultpage/outfit-latin-wght-normal.woff2
var outfitLatin []byte

var resultPage = template.Must(template.New("result").Parse(resultPageHTML))

// The @font-face rule, built once, with the font inline.
var outfitFontFace = template.CSS(`@font-face {
  font-family: "Outfit Variable";
  font-style: normal;
  font-display: swap;
  font-weight: 100 900;
  src: url(data:font/woff2;base64,` + base64.StdEncoding.EncodeToString(outfitLatin) + `) format("woff2-variations");
  unicode-range: U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2191,U+2193,U+2212,U+2215,U+FEFF,U+FFFD;
}`)

const resultPageCSP = "default-src 'none'; style-src 'unsafe-inline'; font-src data:; frame-ancestors 'none'"

// writeResultPage answers the callback with the page: a success for a 200, a failure for any other status.
func writeResultPage(w http.ResponseWriter, status int, heading, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", resultPageCSP)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = resultPage.Execute(w, struct {
		FontFace        template.CSS
		Heading, Detail string
		OK              bool
	}{outfitFontFace, heading, detail, status == http.StatusOK})
}
