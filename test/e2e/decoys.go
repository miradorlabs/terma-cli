package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Decoys: credentials a developer's machine has, planted fake in the sandbox before a files
// profile's run, so the trace shows whether a build touches them, and how (looked at, opened,
// read). Each looks real to a program that parses it and is worthless: a throwaway SSH key made
// for the run, keys and tokens of the right shape that were never issued. Their contents are
// never recorded, only what the build did to each path. Two decoy sockets stand in for an SSH
// agent (SSH_AUTH_SOCK) and Docker (DOCKER_HOST): the suite listens on both and counts who
// connects.

// decoyFiles are the planted files by their path under the home or the repository, and what each
// holds; "<ssh-key>" is a key made for the run. Each token's prefix is joined to it at run time,
// so no secret scanner takes this file for one that leaks a credential.
var decoyFiles = []struct{ home, repo, content string }{
	{home: ".ssh/id_ed25519", content: "<ssh-key>"},
	{home: ".aws/credentials", content: "[default]\naws_access_key_id = " + "AK" + "IAZZZZTERMADECOY00\naws_secret_access_key = terma/decoy/0000000000000000000000000000000\n"},
	{home: ".netrc", content: "machine api.github.com login terma-decoy password " + "gh" + "p_TermaDecoyNotAToken00000000000000000\n"},
	{home: ".config/gh/hosts.yml", content: "github.com:\n    user: terma-decoy\n    oauth_token: " + "gh" + "o_TermaDecoyNotAToken00000000000000000\n    git_protocol: https\n"},
	{home: ".docker/config.json", content: `{"auths":{"https://index.docker.io/v1/":{"auth":"dGVybWEtZGVjb3k6bm90LWEtcGFzc3dvcmQ="}}}` + "\n"},
	{home: ".npmrc", content: "//registry.npmjs.org/:_authToken=" + "np" + "m_TermaDecoyNotAToken000000000000000000\n"},
	{home: ".git-credentials", content: "https://terma-decoy:" + "gh" + "p_TermaDecoyNotAToken00000000000000000@github.com\n"},
	{repo: ".env", content: "OPENAI_API_KEY=" + "sk" + "-terma-decoy-not-a-key-0000000000000\nDATABASE_URL=postgres://decoy:decoy@localhost/decoy\n"},
	{repo: "secrets.json", content: `{"stripe_secret_key":"` + "sk" + `_live_TermaDecoyNotAKey000000000","api_token":"terma-decoy"}` + "\n"},
}

// Decoys are a run's planted credentials.
type Decoys struct {
	// Paths are every decoy's path, files and sockets; Real the sockets' own paths, which
	// their decoy paths link to (a Unix socket's path is short), by decoy path.
	Paths []string
	Real  map[string]string
	// Env points the build at the decoy sockets.
	Env []string

	accepted map[string]*atomic.Int32
}

// PlantDecoys plants the decoys in sb's home and repository, and listens on its sockets for
// the test's lifetime.
func PlantDecoys(t *testing.T, sb *Sandbox) *Decoys {
	t.Helper()
	d := &Decoys{Real: map[string]string{}, accepted: map[string]*atomic.Int32{}}
	for _, f := range decoyFiles {
		path := filepath.Join(sb.Home, f.home)
		if f.repo != "" {
			path = filepath.Join(sb.Repo, f.repo)
		}
		content := f.content
		if content == "<ssh-key>" {
			content = throwawaySSHKey(t)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		d.Paths = append(d.Paths, path)
	}
	// A Unix socket's path has at most 104 bytes on macOS, 108 on Linux: the sockets are made in a
	// short folder, and their decoy paths link to them.
	short, err := os.MkdirTemp("/tmp", "td")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	for _, s := range []struct{ decoy, env string }{
		{filepath.Join(sb.Home, ".ssh", "agent.sock"), "SSH_AUTH_SOCK="},
		{filepath.Join(sb.Home, ".docker", "run", "docker.sock"), "DOCKER_HOST=unix://"},
	} {
		real := filepath.Join(short, filepath.Base(s.decoy))
		ln, err := net.Listen("unix", real)
		if err != nil {
			t.Fatalf("decoy socket: %v", err)
		}
		n := &atomic.Int32{}
		d.accepted[s.decoy] = n
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				n.Add(1)
				_ = c.Close()
			}
		}()
		t.Cleanup(func() { _ = ln.Close() })
		if err := os.MkdirAll(filepath.Dir(s.decoy), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, s.decoy); err != nil {
			t.Fatal(err)
		}
		d.Paths = append(d.Paths, s.decoy)
		d.Real[s.decoy] = real
		d.Env = append(d.Env, s.env+s.decoy)
	}
	return d
}

// Scope adds the decoys to a trace's scope: each socket's own path is its decoy path.
func (d *Decoys) Scope(s *TraceScope) {
	s.Decoys = append(s.Decoys, d.Paths...)
	if s.Replace == nil {
		s.Replace = map[string]string{}
	}
	names := map[string]string{}
	for decoy := range d.Real {
		names[decoy] = s.normalize(decoy)
	}
	for decoy, real := range d.Real {
		name := names[decoy]
		for _, form := range []string{real, "/private" + real} {
			s.Decoys = append(s.Decoys, form)
			s.Replace[form] = name
		}
	}
}

// Connected marks, in probe, the decoy sockets something connected to, whether or not the
// trace saw it.
func (d *Decoys) Connected(probe *FilesProbe, s TraceScope) {
	for decoy, n := range d.accepted {
		if n.Load() > 0 {
			raiseDecoy(probe, s.normalize(decoy), "connected")
		}
	}
}

// throwawaySSHKey is an unencrypted OpenSSH ed25519 private key made now, used for nothing.
func throwawaySSHKey(t *testing.T) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	str := func(b []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
		return append(out, b...)
	}
	pubBlob := append(str([]byte("ssh-ed25519")), str(pub)...)
	var check [4]byte
	_, _ = rand.Read(check[:])
	section := append(append([]byte{}, check[:]...), check[:]...)
	section = append(section, str([]byte("ssh-ed25519"))...)
	section = append(section, str(pub)...)
	section = append(section, str(priv)...)
	section = append(section, str([]byte("terma-decoy"))...)
	for i := byte(1); len(section)%8 != 0; i++ {
		section = append(section, i)
	}
	body := append([]byte("openssh-key-v1\x00"), str([]byte("none"))...)
	body = append(body, str([]byte("none"))...)
	body = append(body, str(nil)...)
	body = binary.BigEndian.AppendUint32(body, 1)
	body = append(body, str(pubBlob)...)
	body = append(body, str(section)...)
	return string(pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: body}))
}
