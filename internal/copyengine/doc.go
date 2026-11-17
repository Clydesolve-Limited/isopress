// Package copyengine implements the copy-set writer with the strict
// per-item ordering of DESIGN_SPEC 5.1 (lstat check, write .partial, fsync,
// atomic rename, read-back verify, mark copied), including retry handling and
// crash recovery via copied flags and .partial cleanup (DESIGN_SPEC 1.3).
package copyengine
