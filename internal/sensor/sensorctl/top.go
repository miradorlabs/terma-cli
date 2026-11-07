package sensorctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/sensor"
)

// Top redraws the sessions on w every second, with each one's rates since the last draw.
func Top(sock string, w io.Writer) error {
	prev := map[int]sensor.SessionView{}
	last := time.Now()
	for {
		var buf bytes.Buffer
		if err := Get(sock, "/v1/sessions", &buf); err != nil {
			return err
		}
		var views []sensor.SessionView
		if err := json.Unmarshal(buf.Bytes(), &views); err != nil {
			return err
		}
		dt := time.Since(last).Seconds()
		last = time.Now()
		var out strings.Builder
		out.WriteString("\x1b[H\x1b[2J")
		fmt.Fprintf(&out, "terma sensor — %s\n\n", time.Now().Format(time.TimeOnly))
		fmt.Fprintf(&out, "%-7s %-10s %5s %10s %10s %9s %9s %8s %8s %4s %4s %4s\n",
			"ROOT", "AGENT", "PROCS", "SENT", "RECV", "↑/s", "↓/s", "FREAD", "FWRITE", "RM", "BLK", "CTR")
		for _, v := range views {
			p := prev[v.Root]
			up, down := 0.0, 0.0
			if p.Root != 0 && dt > 0 {
				up, down = float64(v.TX-min(p.TX, v.TX))/dt, float64(v.RX-min(p.RX, v.RX))/dt
			}
			agent := filepath.Base(v.Agent)
			if !v.End.IsZero() {
				agent += " ✝"
			}
			fmt.Fprintf(&out, "%-7d %-10.10s %5d %10s %10s %9s %9s %8s %8s %4d %4d %4d\n",
				v.Root, agent, v.Procs, size(v.TX), size(v.RX), size(uint64(up)), size(uint64(down)),
				size(v.FilesRead), size(v.FilesWritten), v.Removed, v.Blocks, v.Containers)
			for _, h := range v.Hosts[:min(len(v.Hosts), 4)] {
				live := ""
				if h.Live > 0 {
					live = fmt.Sprintf(" (%d open)", h.Live)
				}
				fmt.Fprintf(&out, "        └ %-44.44s ↑%-9s ↓%-9s %d conns%s\n", h.Host, size(h.TX), size(h.RX), h.Flows+h.Live, live)
			}
			prev[v.Root] = v
		}
		if _, err := io.WriteString(w, out.String()); err != nil {
			return err
		}
		time.Sleep(time.Second)
	}
}

func size(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}
