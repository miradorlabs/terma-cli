// Command sign signs a release file with terma's release key (internal/selfupdate,
// signing.go). The release workflow runs it on checksums.txt through GoReleaser's
// `signs`; a person runs it once, with -generate, to make a key pair.
//
//	go run ./scripts/sign -generate <private-key-file>   # prints the public key
//	TERMA_SIGNING_KEY=<base64 seed> go run ./scripts/sign <file> <signature-file>
//	go run ./scripts/sign -throwaway                     # a random seed, for dry runs
//
// Dry runs (make release-dry-run, make test-install, CI) sign with a throwaway seed, so
// the signing step is exercised on every change without the release key.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 2 && args[0] == "-generate" {
		return generate(args[1])
	}
	if len(args) == 1 && args[0] == "-throwaway" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		fmt.Println(base64.StdEncoding.EncodeToString(priv.Seed()))
		return nil
	}
	if len(args) != 2 {
		return errors.New("usage: sign <file> <signature-file> | sign -generate <private-key-file>")
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("TERMA_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("TERMA_SIGNING_KEY must hold the base64 ed25519 seed")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	sig := selfupdate.Sign(ed25519.NewKeyFromSeed(seed), data)
	// A release (TERMA_SIGNING_REQUIRE_TRUSTED=1, set by release.yml) must be signed by a
	// key terma trusts: a secret holding any other key would publish a release no
	// installed terma accepts, and every update would fail with nothing said at release
	// time. Dry runs sign with a throwaway key and skip this.
	if os.Getenv("TERMA_SIGNING_REQUIRE_TRUSTED") == "1" {
		if err := selfupdate.Verify(data, sig); err != nil {
			return fmt.Errorf("TERMA_SIGNING_KEY is not the release key this build trusts: %w", err)
		}
	}
	return os.WriteFile(args[1], sig, 0o644)
}

func generate(path string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("public key: %s\nkey id:     %s\nprivate key written to %s — store it as the TERMA_SIGNING_KEY secret, then delete the file\n",
		base64.StdEncoding.EncodeToString(pub), selfupdate.KeyID(pub), path)
	return nil
}
