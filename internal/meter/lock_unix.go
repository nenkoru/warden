//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package meter

import (
	"os"
	"syscall"
)

func flock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}

func funlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
