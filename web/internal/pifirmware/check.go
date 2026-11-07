package pifirmware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"runtime"
	"sync"
	"time"
)

// CPUCheck keeps every CPU busy for d, hashing the same buffer over and
// over and checking each answer. An overclock that isn't stable shows up
// as a wrong answer here, or as a crash, which the watchdog catches.
func CPUCheck(ctx context.Context, d time.Duration) error {
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i * 31)
	}
	want := sha256.Sum256(buf)

	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		fail error
	)
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if got := sha256.Sum256(buf); !bytes.Equal(got[:], want[:]) {
					mu.Lock()
					fail = errors.New("a CPU computed a wrong hash")
					mu.Unlock()
					cancel()
					return
				}
			}
		}()
	}
	wg.Wait()
	return fail
}
