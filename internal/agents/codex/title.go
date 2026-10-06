package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Codex names a thread in a hidden side conversation whose answer is never exported; the
// name lands only in $CODEX_HOME/session_index.jsonl, one line per name or rename.

const codexSessionIndex = "session_index.jsonl"

// threadTitle is the name Codex last gave a thread and when it did.
type threadTitle struct {
	Name      string
	UpdatedAt time.Time
}

// readThreadTitle returns the latest name session_index.jsonl holds for threadID;
// found is false before the side conversation answers, and the next capture picks it up.
func readThreadTitle(ctx context.Context, threadID string) (title threadTitle, found bool, err error) {
	home, err := codexHome()
	if err != nil {
		return threadTitle{}, false, err
	}
	f, err := os.Open(filepath.Join(home, codexSessionIndex))
	if errors.Is(err, os.ErrNotExist) {
		return threadTitle{}, false, nil
	}
	if err != nil {
		return threadTitle{}, false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if ctx.Err() != nil {
			return threadTitle{}, false, ctx.Err()
		}
		line := sc.Bytes()
		if !strings.Contains(string(line), threadID) {
			continue
		}
		var rec struct {
			ID         string    `json:"id"`
			ThreadName string    `json:"thread_name"`
			UpdatedAt  time.Time `json:"updated_at"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.ID != threadID {
			continue
		}
		name := strings.TrimSpace(rec.ThreadName)
		if name == "" || (found && rec.UpdatedAt.Before(title.UpdatedAt)) {
			continue
		}
		title, found = threadTitle{Name: name, UpdatedAt: rec.UpdatedAt}, true
	}
	return title, found, sc.Err()
}
