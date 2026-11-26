package style

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal and returns its terminal side and master.
func openPTY() (tty, master *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := int(master.Fd())
	var st unix.Stat_t
	if err = unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err == nil {
		err = unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0)
	}
	if err == nil {
		err = unix.Fstat(fd, &st)
	}
	if err == nil {
		// A master's minor number is its terminal's: /dev/ttys<minor>.
		tty, err = os.OpenFile(fmt.Sprintf("/dev/ttys%03d", unix.Minor(uint64(st.Rdev))), os.O_RDWR|unix.O_NOCTTY, 0)
	}
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return tty, master, nil
}
