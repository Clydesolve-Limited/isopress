// Package fsmeta provides filesystem metadata helpers: lstat-based
// classification and stat capture, change detection (size + mtime_ns + dev +
// inode), metadata application for restore (chmod, lchown, chtimes), and
// symlink and hard-link operations (DESIGN_SPEC 1.3, section 5.4).
package fsmeta
