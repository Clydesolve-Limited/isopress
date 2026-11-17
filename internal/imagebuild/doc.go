// Package imagebuild implements the ISO image production pipeline
// (DESIGN_SPEC 1.3, section 6): the staging workspace and payload
// staging writer (T4.2: whole files, chunk parts, symlinks, empty
// dirs, the two capacity pre-checks, the graft-points path-list), and
// the T4.3 build phase - the xorriso invocation through the T4.1
// Master seam, the 6.6 per-image content-list transaction, the
// extraction-based self-verification (via internal/verify), and the
// 6.3 atomic finalize. See build.go for the numbered 6.3 flow and its
// documented decisions.
package imagebuild
