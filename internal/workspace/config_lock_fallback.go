//go:build aix || js || plan9 || wasip1

package workspace

import "sync"

var fallbackConfigLock sync.Mutex

// These targets do not expose the advisory file-lock APIs used by the CLI's
// desktop/server platforms. Keep goroutine-level safety when they are used.
func lockConfig(string) (func(), error) {
	fallbackConfigLock.Lock()
	return fallbackConfigLock.Unlock, nil
}
