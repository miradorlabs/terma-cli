package e2e

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

// procArgs is the command line and environment process pid was started with (sysctl
// KERN_PROCARGS2), as the kernel keeps them: a process of this user's alone.
func procArgs(pid int) (argv, env []string, err error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, nil, err
	}
	if len(data) < 4 {
		return nil, nil, errors.New("procargs2: too short")
	}
	argc := int(binary.LittleEndian.Uint32(data[:4]))
	rest := data[4:]
	// The executable's path, then NULs to align, then argv and the environment.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, nil, errors.New("procargs2: no executable path")
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	for s := range bytes.SplitSeq(rest, []byte{0}) {
		switch {
		case len(argv) < argc:
			argv = append(argv, string(s))
		case len(s) == 0:
			return argv, env, nil
		default:
			env = append(env, string(s))
		}
	}
	return argv, env, nil
}
