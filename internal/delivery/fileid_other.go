//go:build !unix && !windows

package delivery

func fileIdentity(string) string { return "" }
