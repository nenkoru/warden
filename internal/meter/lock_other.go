//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)

package meter

import "os"

// No flock here: the ledger is safe within one process only.
func flock(*os.File) error   { return nil }
func funlock(*os.File) error { return nil }
