package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// readConfirmation consumes only the current answer: buffering a pipe here would
// discard answers intended for later prompts when the temporary reader is dropped.
func readConfirmation(in io.Reader, singleKey, def bool) (bool, error) {
	var line strings.Builder
	var key [1]byte
	for {
		n, err := in.Read(key[:])
		if n > 0 {
			if singleKey {
				switch key[0] {
				case 'y', 'Y':
					return true, nil
				case 'n', 'N':
					return false, nil
				case '\r', '\n':
					return def, nil
				case 3, 4, 27:
					return false, errCancelled
				}
			} else if key[0] == '\n' {
				return yesAnswer(line.String(), def), nil
			} else {
				line.WriteByte(key[0])
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// An empty pipe never accepts the default, but printf y is valid.
				return strings.TrimSpace(line.String()) != "" && yesAnswer(line.String(), def), nil
			}
			return false, fmt.Errorf("read confirmation: %w", err)
		}
	}
}
