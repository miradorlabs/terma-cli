//go:build windows

package delivery

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// fileIdentity returns a stable identifier for the current OS file instance at path.
// O_APPEND writes by hooks preserve this identity, while spool.ackBatch's atomic rename
// creates a fresh file instance with a new identity.
func fileIdentity(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		info.VolumeSerialNumber,
		info.FileIndexHigh, info.FileIndexLow,
		info.CreationTime.HighDateTime, info.CreationTime.LowDateTime,
	)
}
