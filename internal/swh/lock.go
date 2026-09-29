//go:build darwin || linux

package swh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const lockPoll = 50 * time.Millisecond

func acquireLock(ctx context.Context, cache, id string) (*os.File, error) {
	dir := filepath.Join(cache, ".locks")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, err
	}
	// Keep lock files so waiters always lock the same inode.
	file, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = file.Close()
			return nil, err
		}
		timer := time.NewTimer(lockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
