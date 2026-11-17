// Package verify implements content verification: the write-side
// extraction-and-hash primitive, the verify-media command path (ISO images
// via xorriso extraction, copy trees via direct reads), and the validate
// source-versus-catalog comparison (DESIGN_SPEC 1.3, section 7).
package verify
