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

// CPUCheck keeps every CPU busy for d, each hashing the same size bytes
// over and over and checking each answer. size should be well beyond the
// caches (a few hundred MB), so memory is exercised too. An overclock
// that isn't stable shows up as a wrong answer here, or as a crash, which
// the watchdog catches.
func CPUCheck(ctx context.Context, d time.Duration, size int) error {
	buf := make([]byte, size)
	// xorshift, so the data isn't a repeating pattern.
	x := uint64(0x9e3779b97f4a7c15)
	for i := range buf {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		buf[i] = byte(x)
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
