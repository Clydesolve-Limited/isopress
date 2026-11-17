// Package progress is a one-way event channel from services to a single
// renderer goroutine. It holds the event types and the interactive, plain,
// and silent renderers plus TTY detection; services never write to stderr
// themselves, only the renderer does (DESIGN_SPEC 1.2, 1.3, section 9).
package progress
