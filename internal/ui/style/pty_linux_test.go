package style

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal and returns its terminal side and master.
func openPTY() (tty, master *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := int(master.Fd())
	n, err := 0, unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0)
	if err == nil {
		n, err = unix.IoctlGetInt(fd, unix.TIOCGPTN)
	}
	if err == nil {
		tty, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	}
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return tty, master, nil
}
