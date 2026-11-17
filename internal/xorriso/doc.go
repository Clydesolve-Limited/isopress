// Package xorriso is the external integration layer wrapping xorriso:
// exec.LookPath discovery, version probing against the >= 1.5.4 floor,
// argument construction, execution with output capture, and error
// classification into DependencyError or build failure (DESIGN_SPEC 1.2,
// 1.3, NFR-004). No other package may import os/exec for xorriso.
package xorriso
