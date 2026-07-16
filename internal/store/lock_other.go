//go:build !unix

package store

// Advisory file locking isn't implemented on this platform; concurrent
// jobwatch instances against the same state file are not protected.
func acquireLock(string) (release func(), err error) {
	return func() {}, nil
}
