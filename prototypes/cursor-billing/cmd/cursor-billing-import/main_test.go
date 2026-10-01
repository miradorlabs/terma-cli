package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSessions(t *testing.T) {
	for _, tc := range []struct {
		name, json string
		valid      bool
	}{
		{"lifecycle", `{"version":1,"test_passed":true,"observations":[{"session.id":"c","account_email":"a","hook_event":"sessionStart","turn_id":"c"},{"session.id":"c","hook_event":"sessionEnd","turn_id":"c"}]}`, true},
		{"failed", `{"version":1,"test_passed":false,"observations":[{"session.id":"c"}]}`, false},
		{"account changed", `{"version":1,"test_passed":true,"observations":[{"session.id":"c","account_email":"a"},{"session.id":"c","account_email":"b"}]}`, false},
		{"empty", `{"version":1,"test_passed":true,"observations":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capture.json")
			if err := os.WriteFile(path, []byte(tc.json), 0600); err != nil {
				t.Fatal(err)
			}
			sessions, err := readSessions(path)
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected result: %v", err)
			}
			if tc.valid && (len(sessions) != 1 || sessions[0].ConversationID != "c" || sessions[0].UserEmail != "a" || len(sessions[0].TurnIDs) != 0) {
				t.Fatal("lifecycle generation was incorrectly counted as a turn")
			}
		})
	}
}
