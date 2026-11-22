//go:build !unix

package config

// lockFile does nothing outside Unix yet (v0.4). The check before the rename
// in writeAtomic still catches most races.
func lockFile(string) (func(), error) { return func() {}, nil }
