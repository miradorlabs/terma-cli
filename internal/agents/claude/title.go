package claude

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// Claude Code exports the title it generates in the terminal, but the desktop app names a Code
// tab session in its own process, and a /rename makes no model call: either name lands only in
// the transcript, as a custom-title record beside the ai-title one.

// claudeTitleTail bounds the transcript read: Claude Code appends the session's title records
// again at every turn, so the latest sits near the end of a transcript of any size.
const claudeTitleTail = 1 << 20

// readTranscriptTitle returns the session's latest name in the transcript's last
// claudeTitleTail bytes: a custom-title over the ai-title Claude Code generated, an emptied
// custom-title falling back to it; found is false before either is written.
func readTranscriptTitle(path, sessionID string) (title string, found bool, err error) {
	buf, err := readTranscriptTail(path)
	if err != nil {
		return "", false, err
	}
	var custom, generated string
	for line := range bytes.Lines(buf) {
		if !bytes.Contains(line, []byte(`"type":"custom-title"`)) && !bytes.Contains(line, []byte(`"type":"ai-title"`)) {
			continue
		}
		var rec struct {
			Type        string `json:"type"`
			SessionID   string `json:"sessionId"`
			CustomTitle string `json:"customTitle"`
			AITitle     string `json:"aiTitle"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.SessionID != sessionID {
			continue
		}
		switch rec.Type {
		case "custom-title":
			custom = strings.TrimSpace(rec.CustomTitle)
		case "ai-title":
			generated = strings.TrimSpace(rec.AITitle)
		}
	}
	title = cmp.Or(custom, generated)
	return title, title != "", nil
}

// readRoutedTranscriptTitle scans the transcript so titles are withheld unless every
// recorded cwd belongs exclusively to the current route (and at least one cwd is present).
func readRoutedTranscriptTitle(path, sessionID string, matches func(string) bool) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", false, err
	}
	var custom, generated string
	seen, hasCwd := map[string]bool{}, false
	for br := bufio.NewReader(f); ; {
		line, rerr := br.ReadBytes('\n')
		cwdHint := bytes.Contains(line, []byte(`"cwd"`))
		if len(line) > 0 && (cwdHint || bytes.Contains(line, []byte(`"type":"custom-title"`)) || bytes.Contains(line, []byte(`"type":"ai-title"`))) {
			var rec struct{ Type, SessionID, Cwd, CustomTitle, AITitle string }
			if json.Unmarshal(line, &rec) != nil {
				if rerr == nil && cwdHint {
					return "", false, nil
				}
			} else {
				if rec.Cwd != "" {
					if hasCwd = true; !seen[rec.Cwd] && !matches(rec.Cwd) {
						return "", false, nil
					}
					seen[rec.Cwd] = true
				}
				if rec.SessionID == sessionID && rec.Type == "custom-title" {
					custom = strings.TrimSpace(rec.CustomTitle)
				} else if rec.SessionID == sessionID && rec.Type == "ai-title" {
					generated = strings.TrimSpace(rec.AITitle)
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				t := cmp.Or(custom, generated)
				return t, hasCwd && t != "", nil
			}
			return "", false, rerr
		}
	}
}

// readTranscriptTail returns the whole lines in the transcript's last claudeTitleTail bytes, or
// none before the transcript exists.
func readTranscriptTail(path string) ([]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, err
	}
	off := max(info.Size()-claudeTitleTail, 0)
	buf := make([]byte, info.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if off > 0 {
		// The tail starts inside a line.
		_, buf, _ = bytes.Cut(buf, []byte("\n"))
	}
	return buf, nil
}
