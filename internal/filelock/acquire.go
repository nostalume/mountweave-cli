package filelock

import (
	"context"
	"os"
	"time"
)

// Acquire waits for an exclusive lock or for ctx to end.
// With a nil context, it attempts the lock once without waiting.
func Acquire(ctx context.Context, file *os.File) (bool, error) {
	for {
		if ctx != nil && ctx.Err() != nil {
			return false, ctx.Err()
		}
		ok, err := Try(file)
		if ok || err != nil || ctx == nil {
			return ok, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
