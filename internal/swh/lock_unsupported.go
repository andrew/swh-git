//go:build !darwin && !linux

package swh

import (
	"context"
	"errors"
	"os"
)

func acquireLock(_ context.Context, _, _ string) (*os.File, error) {
	return nil, errors.New("cache locking requires Linux or macOS")
}
