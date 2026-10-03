//go:build !unix

package registry

// registerDirStores registers nothing: the directory stores are built on unix
// systems only.
func registerDirStores(*Registry) {}
