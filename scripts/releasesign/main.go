// releasesign makes and uses the release signing key: the release workflow signs
// checksums.txt with it (GoReleaser's signs step), and terma installs only a release
// whose checksums.txt a pinned public key signed (internal/selfupdate).
//
//	go run ./scripts/releasesign keygen <keyfile>          writes the private key, prints the public key
//	go run ./scripts/releasesign sign <file> <sigfile>     signs file with $TERMA_SIGNING_KEY
//	go run ./scripts/releasesign verify <file> <sigfile> [<public key hex>]
//
// The private key file holds the 32-byte seed as hex, which TERMA_SIGNING_KEY holds too.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "releasesign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: releasesign keygen <keyfile> | sign <file> <sigfile> | verify <file> <sigfile> [<public key hex>]")
	}
	switch args[0] {
	case "keygen":
		if len(args) != 2 {
			return errors.New("usage: releasesign keygen <keyfile>")
		}
		return keygen(args[1])
	case "sign":
		if len(args) != 3 {
			return errors.New("usage: releasesign sign <file> <sigfile>")
		}
		return sign(args[1], args[2])
	case "verify":
		if len(args) != 3 && len(args) != 4 {
			return errors.New("usage: releasesign verify <file> <sigfile> [<public key hex>]")
		}
		return verify(args[1:])
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// keygen writes a new private key to path, readable by its owner alone, and prints the
// public key to commit. The private key never reaches the terminal.
func keygen(path string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, hex.EncodeToString(priv.Seed())); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("private key written to %s (the TERMA_SIGNING_KEY secret)\npublic key: %s\n", path, hex.EncodeToString(pub))
	return nil
}

// privateKey is TERMA_SIGNING_KEY: the seed, hex.
func privateKey() (ed25519.PrivateKey, error) {
	seed, err := hex.DecodeString(strings.TrimSpace(os.Getenv("TERMA_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("TERMA_SIGNING_KEY must hold the private key's 32-byte seed as hex (releasesign keygen)")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func sign(path, sigPath string) error {
	priv, err := privateKey()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(sigPath, selfupdate.Sign(priv, data), 0o644)
}

// verify checks a signature against the given public key, else the keys built into terma.
func verify(args []string) error {
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	keys := selfupdate.BuiltinKeys()
	if len(args) == 3 {
		k, err := selfupdate.ParseKey(args[2])
		if err != nil {
			return err
		}
		keys = []ed25519.PublicKey{k}
	}
	if err := selfupdate.Verify(keys, data, sig); err != nil {
		return err
	}
	fmt.Printf("%s: signature valid\n", args[0])
	return nil
}
