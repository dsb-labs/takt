//go:build !linux

// Package exec provides the driver that runs workloads as processes on the host.
package exec

// Confine does nothing: the confinement trampoline only exists on linux, where
// the exec driver runs.
func Confine() {}
