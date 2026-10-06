//go:build !windows

package config

import "os"

// Rename is os.Rename; only Windows refuses to move a file someone has open.
func Rename(from, to string) error { return os.Rename(from, to) }

// Remove is os.Remove; only Windows refuses to delete a file someone has open.
func Remove(path string) error { return os.Remove(path) }
