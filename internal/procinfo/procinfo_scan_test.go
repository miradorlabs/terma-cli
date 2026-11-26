//go:build darwin || linux

package procinfo

func init() { ownsConnForTest, eldestOwnerForTest = ownsConn, eldestOwner }
