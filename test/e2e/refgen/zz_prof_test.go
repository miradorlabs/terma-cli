package main

import (
	"os"
	"testing"
	"time"
)

func TestZZProfRender(t *testing.T) {
	dir := os.Getenv("ZZ_DIR")
	if dir == "" {
		t.Skip()
	}
	n := night{now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), labels: map[string]string{}}
	start := time.Now()
	if _, err := publish(dir, n, options{}); err != nil {
		t.Fatal(err)
	}
	t.Logf("publish %v", time.Since(start))
}
