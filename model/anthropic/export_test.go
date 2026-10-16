package anthropic

import "time"

// SetWindowWait sets how long Window waits, until the returned func restores it.
func SetWindowWait(d time.Duration) func() {
	old := windowWait
	windowWait = d
	return func() { windowWait = old }
}
