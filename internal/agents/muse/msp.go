package muse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Muse's session protocol (MSP) is newline-delimited JSON-RPC over `muse serve`'s stdin
// and stdout. terma only reads: session/list and view/page, which take no lease, load no
// session and write nothing.

// mspClientName is terma's name to the host, a machine identifier as MSP requires.
const mspClientName = "terma"

// mspCallTimeout bounds one call; a host that stops answering is restarted.
const mspCallTimeout = 30 * time.Second

// mspHost is one `muse serve` process terma reads through.
type mspHost struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan mspReply
}

type mspReply struct {
	Result json.RawMessage
	Err    error
}

// startMSPHost starts `muse serve` from the launcher on PATH, auto-update off so a read
// never waits on a download, and opens the connection.
func startMSPHost(ctx context.Context, version string) (*mspHost, error) {
	muse, err := exec.LookPath("muse")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(muse, "serve")
	cmd.Env = append(os.Environ(), "MUSE_NO_AUTO_UPDATE=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h := &mspHost{cmd: cmd, stdin: stdin, done: make(chan struct{}), pending: map[int64]chan mspReply{}}
	go h.read(stdout)
	var init struct {
		ServerInfo struct {
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	params := map[string]any{"clientInfo": map[string]string{"name": mspClientName, "version": version}, "capabilities": map[string]any{}}
	if err := h.call(ctx, "initialize", params, &init); err != nil {
		h.close()
		return nil, fmt.Errorf("muse serve: initialize: %w", err)
	}
	if err := h.notify("initialized", map[string]any{}); err != nil {
		h.close()
		return nil, err
	}
	return h, nil
}

// read routes each reply to its call; notifications, the host's own news, are not read.
func (h *mspHost) read(stdout io.Reader) {
	defer close(h.done)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var msg struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID == nil {
			continue
		}
		h.mu.Lock()
		ch := h.pending[*msg.ID]
		delete(h.pending, *msg.ID)
		h.mu.Unlock()
		if ch == nil {
			continue
		}
		reply := mspReply{Result: msg.Result}
		if msg.Error != nil {
			reply.Err = fmt.Errorf("%s (%d)", msg.Error.Message, msg.Error.Code)
		}
		ch <- reply
	}
}

func (h *mspHost) send(msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err = h.stdin.Write(append(b, '\n'))
	return err
}

func (h *mspHost) notify(method string, params any) error {
	return h.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// call sends one request and decodes its result into out.
func (h *mspHost) call(ctx context.Context, method string, params, out any) error {
	ch := make(chan mspReply, 1)
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.pending[id] = ch
	h.mu.Unlock()
	if err := h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, mspCallTimeout)
	defer cancel()
	select {
	case r := <-ch:
		if r.Err != nil {
			return r.Err
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(r.Result, out)
	case <-h.done:
		return errors.New("muse serve exited")
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return ctx.Err()
	}
}

// close ends the host: its stdin closed, then a signal if it lingers.
func (h *mspHost) close() {
	_ = h.stdin.Close()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
	}
	_ = h.cmd.Wait()
}
