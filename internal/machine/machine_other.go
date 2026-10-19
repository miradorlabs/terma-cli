//go:build !darwin

package machine

// localName is "": elsewhere os.Hostname is the name the machine was configured with (Linux's
// /etc/hostname, Windows' computer name), not one the network hands out.
func localName() string { return "" }
