// Package ingest implements the scan pipeline: directory walk, exclusion
// matcher, hash worker pool (SHA-256), and the rescan reconciler that handles
// new, unchanged, modified, and missing paths transactionally
// (DESIGN_SPEC 1.3, sections 3.1 to 3.7).
package ingest
