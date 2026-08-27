// Package lockadapter serializes cross-process access to shared cache
// state. One advisory file lock per key: two processes touching the same
// mirror or run root - this workspace's, a second workspace's, or an
// orphaned child's - wait on each other instead of racing on git refs.
package lockadapter

// Locker takes a blocking exclusive lock keyed by a path and answers the
// release. Declared here, in the package that implements it.
type Locker interface {
	Lock(path string) (func(), error)
}
