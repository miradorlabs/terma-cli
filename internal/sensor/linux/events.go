//go:build linux

package linux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/cilium/ebpf/ringbuf"

	"github.com/miradorlabs/terma-cli/internal/sensor"
)

// Event types, as sensor.bpf.c numbers them.
const (
	evExec = iota + 1
	evFork
	evExit
	evFlow
	evFile
	evUnlink
	evRename
	evBlock
)

// Run reads the programs' events into sink until ctx ends.
func (b *Backend) Run(ctx context.Context, sink sensor.Sink) error {
	rd, err := ringbuf.NewReader(b.objs.Events)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = rd.Close()
	}()
	for {
		rec, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return ctx.Err()
			}
			return err
		}
		if err := b.dispatch(rec.RawSample, sink); err != nil {
			return err
		}
	}
}

func (b *Backend) dispatch(raw []byte, sink sensor.Sink) error {
	if len(raw) < 4 {
		return nil
	}
	read := func(v any) error { return binary.Read(bytes.NewReader(raw), binary.LittleEndian, v) }
	switch binary.LittleEndian.Uint32(raw) {
	case evExec:
		var e sensorEventExec
		if err := read(&e); err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		p := b.proc(&e.H)
		p.Exe = cstr(e.Filename[:])
		p.Args = splitArgs(e.Args[:min(int(e.ArgsLen), len(e.Args))])
		sink.Exec(p)
	case evFork:
		var h sensorHeader
		if err := read(&h); err != nil {
			return err
		}
		sink.Fork(b.proc(&h))
	case evExit:
		var h sensorHeader
		if err := read(&h); err != nil {
			return err
		}
		sink.Exit(int(h.Tgid), int(h.Root), b.at(h.Ts))
	case evFlow:
		var e sensorEventFlow
		if err := read(&e); err != nil {
			return err
		}
		f := b.flow(0, &e.F)
		f.Closed = true
		f.Last = b.at(e.H.Ts)
		sink.FlowClosed(f)
	case evFile:
		var e sensorEventFile
		if err := read(&e); err != nil {
			return err
		}
		sink.File(sensor.FileOp{
			Op: sensor.OpClose, PID: int(e.F.Tgid), Root: int(e.F.Root), Comm: cstr(e.H.Comm[:]),
			Path: cstr(e.F.Path[:]), Read: e.F.Rx, Written: e.F.Tx, Flags: e.F.Flags, Time: b.at(e.H.Ts),
		})
	case evUnlink, evRename:
		var e sensorEventPath
		if err := read(&e); err != nil {
			return err
		}
		op := sensor.FileOp{PID: int(e.H.Tgid), Root: int(e.H.Root), Comm: cstr(e.H.Comm[:]), Time: b.at(e.H.Ts)}
		if binary.LittleEndian.Uint32(raw) == evUnlink {
			op.Op = sensor.OpUnlink
			op.Path = strings.TrimSuffix(join(e.To.Name[:], e.To.Depth), "/") + "/" + cstr(e.From.Name[0][:])
		} else {
			op.Op = sensor.OpRename
			op.Path = join(e.From.Name[:], e.From.Depth)
			op.To = join(e.To.Name[:], e.To.Depth)
		}
		sink.File(op)
	case evBlock:
		var e sensorEventBlock
		if err := read(&e); err != nil {
			return err
		}
		blk := sensor.Block{
			PID: int(e.H.Tgid), Root: int(e.H.Root), Comm: cstr(e.H.Comm[:]), Rule: int(e.Rule),
			Enforced: e.Enforced != 0, Time: b.at(e.H.Ts),
		}
		if e.Kind == 1 {
			blk.Kind = "connect"
			blk.Target = fmt.Sprintf("%s:%d", addr(e.Family, e.Addr), e.Port)
		} else {
			blk.Kind = "open"
			blk.Target = cstr(e.Path[:])
		}
		sink.Blocked(blk)
	}
	return nil
}

func (b *Backend) proc(h *sensorHeader) sensor.Proc {
	return sensor.Proc{
		PID: int(h.Tgid), PPID: int(h.Ppid), Root: int(h.Root), Comm: cstr(h.Comm[:]),
		Cgrp: h.Cgroup, Start: b.at(h.Ts),
	}
}

// join turns names from the leaf up into a path.
func join(names [][64]int8, depth uint32) string {
	parts := make([]string, 0, depth)
	for i := int(min(depth, uint32(len(names)))) - 1; i >= 0; i-- {
		parts = append(parts, cstr(names[i][:]))
	}
	return "/" + strings.Join(parts, "/")
}

func splitArgs(b []int8) []string {
	var out []string
	for _, a := range strings.Split(cstr8(b), "\x00") {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

// cstr8 keeps the NULs between arguments that cstr would stop at.
func cstr8(b []int8) string {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = byte(c)
	}
	return string(out)
}
