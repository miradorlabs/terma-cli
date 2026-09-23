//go:build !unix

package flock

import "context"

func lock(ctx context.Context, _ string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() {}, nil
}

func tryLock(string) (func(), error) { return func() {}, nil }

func isBusy(error) bool { return false }
