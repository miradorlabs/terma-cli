//go:build !windows

package service

import "errors"

// The Run key and its supervisor (manager_windows.go) exist only on Windows.

func (Manager) installWindows(string) error { return errors.New("not Windows") }

func (Manager) removeWindows(string) error { return errors.New("not Windows") }

func (Manager) startWindows() error { return errors.New("not Windows") }
