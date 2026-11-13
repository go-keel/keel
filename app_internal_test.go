// In-package: drives watchSecondSignal with a fake signal channel; the public API reacts only to real OS signals.

package keel

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// The first signal leaves the graceful stop to kratos; only a second one forces the exit, with code 130.
func TestWatchSecondSignal(t *testing.T) {
	c := make(chan os.Signal, 2)
	exited := make(chan int, 1)
	stop := watchSecondSignal(c, func(code int) { exited <- code })
	defer stop()

	c <- syscall.SIGINT
	select {
	case code := <-exited:
		t.Fatalf("exit(%d) after the first signal, want no exit", code)
	case <-time.After(50 * time.Millisecond):
	}

	c <- syscall.SIGINT
	select {
	case code := <-exited:
		if code != 130 {
			t.Fatalf("exit code = %d, want 130", code)
		}
	case <-time.After(time.Second):
		t.Fatal("no exit after the second signal")
	}
}
