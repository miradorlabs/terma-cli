package e2e

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A Chromium's DevTools protocol, for a desktop app whose window a scenario types into (the
// Codex app, started with --remote-debugging-port): the page's script is run as the page's,
// and keys and text are input as a person's would be. A WebSocket client of its own, as small
// as the protocol needs: it sends no Origin, which a Chromium refuses from a client it was not
// told to allow.

// CDP is one page's DevTools session.
type CDP struct {
	conn net.Conn
	r    *bufio.Reader
	id   int
}

// DialCDP connects to the first page on the DevTools port whose URL match accepts, waiting
// up to timeout for it.
func DialCDP(port int, match func(url string) bool, timeout time.Duration) (*CDP, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ws, err := cdpPage(port, match)
		if err == nil {
			return dialWebSocket(ws)
		}
		last = err
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("no DevTools page on port %d: %v", port, last)
}

func cdpPage(port int, match func(string) bool) (string, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var pages []struct {
		Type, URL            string
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pages); err != nil {
		return "", err
	}
	for _, p := range pages {
		if p.Type == "page" && match(p.URL) && p.WebSocketDebuggerURL != "" {
			return p.WebSocketDebuggerURL, nil
		}
	}
	return "", errors.New("no such page yet")
}

func dialWebSocket(raw string) (*CDP, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", u.Host, 10*time.Second)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		u.RequestURI(), u.Host, base64.StdEncoding.EncodeToString(key))
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("DevTools refused the connection: %s", resp.Status)
	}
	return &CDP{conn: conn, r: r}, nil
}

// Close ends the session.
func (c *CDP) Close() { _ = c.conn.Close() }

func (c *CDP) send(payload []byte) error {
	frame := []byte{0x81} // a final text frame
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n < 1<<16:
		frame = append(frame, 0x80|126, byte(n>>8), byte(n))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := c.conn.Write(frame)
	return err
}

func (c *CDP) receive() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.r, h[:]); err != nil {
			return nil, err
		}
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.r, ext[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.r, ext[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(ext[:])
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(c.r, data); err != nil {
			return nil, err
		}
		switch h[0] & 0x0f {
		case 0x8:
			return nil, io.EOF
		case 0x9, 0xa: // ping, pong
			continue
		}
		msg = append(msg, data...)
		if h[0]&0x80 != 0 {
			return msg, nil
		}
	}
}

// Call runs one DevTools method and returns its result.
func (c *CDP) Call(method string, params any) (json.RawMessage, error) {
	c.id++
	id := c.id
	data, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := c.send(data); err != nil {
		return nil, err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{})
	for {
		msg, err := c.receive()
		if err != nil {
			return nil, err
		}
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &reply) != nil || reply.ID != id {
			continue // an event
		}
		if reply.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, reply.Error.Message)
		}
		return reply.Result, nil
	}
}

// Eval runs expression in the page and returns its value.
func (c *CDP) Eval(expression string) (any, error) {
	raw, err := c.Call("Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true})
	if err != nil {
		return nil, err
	}
	var res struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		return nil, errors.New(res.ExceptionDetails.Text)
	}
	return res.Result.Value, nil
}

// Text is the page's visible text.
func (c *CDP) Text() string {
	v, _ := c.Eval(`document.body ? document.body.innerText : ""`)
	s, _ := v.(string)
	return s
}

// Type inputs text where the page's focus is, as a keyboard would.
func (c *CDP) Type(text string) error {
	_, err := c.Call("Input.insertText", map[string]any{"text": text})
	return err
}

// Enter presses the Return key.
func (c *CDP) Enter() error {
	for _, typ := range []string{"keyDown", "keyUp"} {
		p := map[string]any{"type": typ, "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13}
		if typ == "keyDown" {
			p["text"] = "\r"
		}
		if _, err := c.Call("Input.dispatchKeyEvent", p); err != nil {
			return err
		}
	}
	return nil
}

// WaitText waits up to timeout for the page's text to hold want.
func (c *CDP) WaitText(want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(c.Text(), want) {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}
