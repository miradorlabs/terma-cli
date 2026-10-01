//go:build windows

package procinfo

import (
	"encoding/binary"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Supported is true: a process snapshot names parents, and the TCP table names each
// connection's owner.
const Supported = true

// A Toolhelp snapshot is kept briefly: an ancestry walk asks once per step.
var snap struct {
	mu      sync.Mutex
	at      time.Time
	parents map[int]int
}

const snapTTL = time.Second

// parentOf stops at a parent that started after its child: Windows reuses an exited parent's pid.
func parentOf(pid int) (int, bool) {
	parents := processParents()
	ppid, ok := parents[pid]
	if !ok || ppid <= 4 { // 0 is the idle process, 4 the kernel's System
		return 0, false
	}
	if _, alive := parents[ppid]; !alive {
		return 0, false
	}
	child, okc := startTime(pid)
	parent, okp := startTime(ppid)
	if okc && okp && parent.After(child) {
		return 0, false
	}
	return ppid, true
}

func processParents() map[int]int {
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if snap.parents != nil && time.Since(snap.at) < snapTTL {
		return snap.parents
	}
	parents := map[int]int{}
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return parents
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(h, &e); err == nil; err = windows.Process32Next(h, &e) {
		parents[int(e.ProcessID)] = int(e.ParentProcessID)
	}
	snap.parents, snap.at = parents, time.Now()
	return parents
}

func startTime(pid int) (time.Time, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return time.Time{}, false
	}
	return time.Unix(0, created.Nanoseconds()), true
}

var getExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

const (
	afInet                      = 2
	afInet6                     = 23
	tcpTableOwnerPIDConnections = 4
	// Row sizes of MIB_TCPROW_OWNER_PID and MIB_TCP6ROW_OWNER_PID, and offsets in each.
	row4Size, row4LocalPort, row4PID = 24, 8, 20
	row6Size, row6LocalPort, row6PID = 56, 20, 52
)

// findSender finds the TCP table row whose local port is port, IPv4 then IPv6.
func findSender(port, self int) (int, bool) {
	for _, t := range []struct{ af, size, localPort, pid int }{
		{afInet, row4Size, row4LocalPort, row4PID},
		{afInet6, row6Size, row6LocalPort, row6PID},
	} {
		table, ok := tcpTable(t.af)
		if !ok || len(table) < 4 {
			continue
		}
		n := int(binary.LittleEndian.Uint32(table))
		rows := table[4:]
		// IPv4 rows follow the count directly; IPv6 rows are aligned the same way.
		for i := range n {
			row := rows[i*t.size:]
			if len(row) < t.size {
				break
			}
			// Ports sit in the low 16 bits of a DWORD, in network byte order.
			local := int(binary.BigEndian.Uint16(row[t.localPort:]))
			pid := int(binary.LittleEndian.Uint32(row[t.pid:]))
			if local == port && pid != self && pid > 4 {
				return pid, true
			}
		}
	}
	return 0, false
}

// tcpTable returns af's TCP_TABLE_OWNER_PID_CONNECTIONS table, sized by asking first.
func tcpTable(af int) ([]byte, bool) {
	if getExtendedTCPTable.Find() != nil {
		return nil, false
	}
	size := uint32(64 << 10)
	for range 4 {
		buf := make([]byte, size)
		ret, _, _ := getExtendedTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(af), tcpTableOwnerPIDConnections, 0)
		switch windows.Errno(ret) {
		case 0:
			return buf[:size], true
		case windows.ERROR_INSUFFICIENT_BUFFER:
			size += 4 << 10 // connections come and go between the two calls
		default:
			return nil, false
		}
	}
	return nil, false
}
