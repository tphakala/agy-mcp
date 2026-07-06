//go:build !linux && !windows

package supervisor

// waitForCancel is never reached on unsupported platforms: Run refuses via
// proc.Supported before the cancel wait. It returns a channel that never fires so
// the package compiles.
func waitForCancel(_ string) (cancel <-chan struct{}, stop func()) {
	return make(chan struct{}), func() {}
}
