//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package workspace

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockConfig serializes the complete read-modify-rename sequence across
// processes. The separate lock file is stable while the config itself is
// atomically replaced with rename.
func lockConfig(path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, "."+filepath.Base(path)+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
