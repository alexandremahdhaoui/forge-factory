//go:build windows

package lockadapter

// Flock is a no-op on windows: the cache race this guards against is a
// posix-runner concern, and the toolchain's distribution targets linux.
type Flock struct{}

var _ Locker = (*Flock)(nil)

func New() *Flock { return &Flock{} }

func (f *Flock) Lock(string) (func(), error) { return func() {}, nil }
