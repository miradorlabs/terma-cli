//go:build linux

package linux

import (
	"context"
	"encoding/binary"

	"golang.org/x/sys/unix"

	"github.com/miradorlabs/terma-cli/internal/sensor"
)

// CaptureNames reads the packets that name hosts (DNS answers, TLS ClientHellos) on every
// interface of the sensor's network namespace, filtered in the kernel by names_filter, and
// hands the names to sink. Run in the root namespace it also sees containers' bridged
// traffic; a container that resolves through its own namespace's DNS is named by its TLS
// hellos alone.
func (b *Backend) CaptureNames(ctx context.Context, sink sensor.Sink) error {
	proto := int(htons(unix.ETH_P_ALL))
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, proto)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = unix.Close(fd)
	}()
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ATTACH_BPF, b.objs.NamesFilter.FD()); err != nil {
		return err
	}
	buf := make([]byte, 65536)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if n < 14 {
			continue
		}
		switch binary.BigEndian.Uint16(buf[12:]) {
		case unix.ETH_P_IP, unix.ETH_P_IPV6:
			if p, ok := sensor.ParseIP(buf[14:n]); ok {
				sensor.Names(p, sink)
			}
		}
	}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }
