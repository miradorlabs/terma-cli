//go:build unix

package cli

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A sign-out that cannot remove the stored sign-in revokes nothing: the copy left could be
// spent by a refresh after the lock, and revoking a spent token ends the whole family.
func TestASignOutThatCannotDeleteRevokesNothing(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := config.UpdateFile(testApp.dir, func(file *config.File) { file.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(testApp.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(testApp.dir, 0o700) })
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := testApp.signOut(cmd); err == nil {
		t.Fatalf("a sign-out that kept the sign-in succeeded:\n%s", &out)
	}
	if f.revokes.Load() != 0 {
		t.Fatalf("%d revokes of a sign-in still stored", f.revokes.Load())
	}
}
