// Package cli holds the cobra command definitions and flag wiring only.
//
// Per DESIGN_SPEC 1.2, this layer parses flags, builds a service context
// (catalog handle, progress emitter, logger), invokes exactly one service
// operation, maps the result to an exit code, and prints the machine-parseable
// result to stdout. No business logic lives here.
package cli
