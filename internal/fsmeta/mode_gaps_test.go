package fsmeta

// T3.4 QA gap-closer (test-only): the inverse-identity proof in
// fsmeta_test.go is genuinely exhaustive over the permission-class
// subspace (all 4096 raw values in [0, 0o7777], plus all 4096
// perm|special fs.FileMode combinations). What it does NOT cover is the
// TYPE-BIT boundary: fs.FileMode values the writer or capture path can
// carry that also embed fs.ModeDir/ModeSymlink/etc. The contract is:
// UnixMode strips type bits (documented loss), and GoMode must map them
// to NOTHING - a type bit must never be mis-mapped into a permission or
// special bit (and vice versa). This test pins that stripping
// deterministically over the full type-bit x perm x special cross.

import (
	"io/fs"
	"syscall"
	"testing"
)

func TestGoModeStripsGoTypeBitsDeterministically(t *testing.T) {
	typeBits := []fs.FileMode{
		0,
		fs.ModeDir,
		fs.ModeSymlink,
		fs.ModeNamedPipe,
		fs.ModeDevice,
		fs.ModeDevice | fs.ModeCharDevice,
		fs.ModeSocket,
		fs.ModeIrregular,
		fs.ModeDir | fs.ModeSticky, // a type bit AND a special bit set at once: Go type and special bits occupy DISJOINT flag positions, so this is a stripping-correctness case (the type bit must strip, the special bit must survive), NOT an aliasing case
	}
	specials := []fs.FileMode{
		0,
		fs.ModeSetuid,
		fs.ModeSetgid,
		fs.ModeSticky,
		fs.ModeSetuid | fs.ModeSetgid,
		fs.ModeSetuid | fs.ModeSticky,
		fs.ModeSetgid | fs.ModeSticky,
		fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky,
	}
	// The permission-class subspace: everything outside this mask is a
	// type bit that UnixMode documents as stripped.
	mask := fs.FileMode(0o777) | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

	for _, tb := range typeBits {
		for perm := fs.FileMode(0); perm <= 0o777; perm++ {
			for _, sp := range specials {
				m := tb | perm | sp
				if got, want := GoMode(UnixMode(m)), m&mask; got != want {
					t.Fatalf("GoMode(UnixMode(%v)) = %v, want %v (type bits %v must strip, never migrate into perm/special bits)",
						m, got, want, m&^mask)
				}
			}
		}
	}
}

// The raw direction with file-type bits in the input (the shape st.Mode
// carries when a caller forgets to mask, e.g. S_IFLNK|0o777): GoMode
// must ignore the S_IF bits, and the permission-class round-trip must
// stay exact.
func TestGoModeRawInputFileTypeBitsIgnored(t *testing.T) {
	for _, raw := range []uint32{
		syscall.S_IFREG | 0o4755,
		syscall.S_IFLNK | 0o777,
		syscall.S_IFDIR | 0o1777,
		syscall.S_IFBLK | 0o660,
		syscall.S_IFCHR | 0o600,
		syscall.S_IFSOCK | 0o755,
		syscall.S_IFIFO | 0o644,
		syscall.S_IFMT | 0o7777, // all type bits set at once
	} {
		want := raw & 0o7777
		if got := UnixMode(GoMode(raw)); got != want {
			t.Fatalf("UnixMode(GoMode(%#o)) = %#o, want %#o (S_IF bits ignored, permission class lossless)", raw, got, want)
		}
	}
}
