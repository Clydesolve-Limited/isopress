package cli

// T6.2 ADDENDUM tests (the reviewer-specified one-liners F3 and F2 of
// the T6.1 review - SEPARATE from the T6.2 golden-schema scope; see
// WORK_LOG's T6.2 entry). Both live at the same site: root.go's custom
// help command (the N7 rewrite of T6.1).
//
//   - F3: 'help <cmd>' must print the SAME help as '<cmd> --help',
//     including the "-h, --help" flags block the custom command used
//     to omit (the target never executed, so cobra never called
//     InitDefaultHelpFlag on it). root.go now calls
//     target.InitDefaultHelpFlag() before target.Help(), exactly as
//     cobra's built-in help command does.
//   - F2: 'help <TAB>' must offer command names - the cobra builtin
//     parity the custom command dropped (it answered with no
//     candidates). The T6.1 pin that recorded the honest
//     no-candidates behavior lives in taxonomy_table_test.go and is
//     flipped there with a deliberate-change comment.

import (
	"strings"
	"testing"
)

// TestHelpTopicMatchesDirectHelpFlagBlock is the F3 pin: for EVERY
// command of the 1.1 tree, `help <cmd>` and `<cmd> --help` must agree on the "-h,
// --help" flags-block entry (the pre-F3 output of `help <cmd>` lacked
// it), and - stronger - the two outputs must be byte-identical: both
// render the SAME command's help through the same template, so any
// divergence means the topic path is rendering a different command
// state than the direct path.
func TestHelpTopicMatchesDirectHelpFlagBlock(t *testing.T) {
	// ALL NINE commands of the 1.1 tree (the review's spot-check
	// extension: the F3 fix lives at one shared site in root.go, so
	// parity must hold for every command, not just the three first
	// pinned).
	for _, name := range stubCommands {
		t.Run(name, func(t *testing.T) {
			direct, stderrDirect, codeDirect := runInProcess(t, name, "--help")
			if codeDirect != 0 {
				t.Fatalf("%s --help exit = %d, want 0 (stderr: %s)", name, codeDirect, stderrDirect)
			}
			topic, stderrTopic, codeTopic := runInProcess(t, "help", name)
			if codeTopic != 0 {
				t.Fatalf("help %s exit = %d, want 0 (stderr: %s)", name, codeTopic, stderrTopic)
			}

			// THE F3 REGRESSION: both spellings must carry the help
			// flags block. Before F3 the topic spelling omitted it.
			for spelling, out := range map[string]string{
				name + " --help": direct,
				"help " + name:   topic,
			} {
				if !strings.Contains(out, "-h, --help") {
					t.Errorf("%q output lacks the -h, --help flags block:\n%s", spelling, out)
				}
			}
			// Flag-block PARITY in both directions: neither spelling may
			// carry an "-h, --help" entry the other lacks (the diff the
			// reviewer asked for, phrased as presence equality).
			if strings.Contains(direct, "-h, --help") != strings.Contains(topic, "-h, --help") {
				t.Errorf("flags-block presence differs between %q and %q:\ndirect:\n%s\ntopic:\n%s",
					name+" --help", "help "+name, direct, topic)
			}
			// Full output parity (stronger): the same command, the same
			// template, the same initialized flags -> identical bytes.
			if direct != topic {
				t.Errorf("%q and %q outputs differ:\ndirect:\n%s\ntopic:\n%s",
					name+" --help", "help "+name, direct, topic)
			}
		})
	}
}

// TestHelpBareTopicCarriesRootFlagsBlock is the root-position half of
// F3: bare `help` renders the ROOT help, which must carry the same
// flags block `isopress --help` shows.
func TestHelpBareTopicCarriesRootFlagsBlock(t *testing.T) {
	direct, _, codeDirect := runInProcess(t, "--help")
	if codeDirect != 0 {
		t.Fatalf("--help exit = %d, want 0", codeDirect)
	}
	topic, _, codeTopic := runInProcess(t, "help")
	if codeTopic != 0 {
		t.Fatalf("bare help exit = %d, want 0", codeTopic)
	}
	if !strings.Contains(topic, "-h, --help") {
		t.Errorf("bare 'help' output lacks the -h, --help flags block:\n%s", topic)
	}
	if direct != topic {
		t.Errorf("'--help' and bare 'help' root outputs differ:\ndirect:\n%s\ntopic:\n%s", direct, topic)
	}
}

// TestHelpCompletionOffersCommandNames is the F2 pin: `help <TAB>`
// (the shell's `__complete help ""`) offers the command names, prefix
// -filtered, with the NoFileComp directive - cobra's built-in parity
// the N7 custom command dropped. The T6.1-era pin of the honest
// no-candidates behavior is flipped in
// TestHelpTopicAndCompletionBlastRadius (taxonomy_table_test.go).
func TestHelpCompletionOffersCommandNames(t *testing.T) {
	// help <TAB>: every command of the 1.1 tree, plus the help command
	// itself (cobra's builtin offers `subCmd == cmd.helpCommand` too).
	stdout, stderr, code := runInProcess(t, "__complete", "help", "")
	if code != 0 {
		t.Fatalf("__complete help exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, cand := range append(append([]string(nil), stubCommands...), "help") {
		if !strings.Contains(stdout, cand+"\t") && !strings.Contains(stdout, cand+"\n") {
			t.Errorf("__complete help output lacks the %q candidate:\n%s\nstderr: %s", cand, stdout, stderr)
		}
	}
	if !strings.Contains(stdout, ":4") { // ShellCompDirectiveNoFileComp
		t.Errorf("__complete help output lacks the NoFileComp directive line:\n%s", stdout)
	}

	// Prefix filtering: `help sca<TAB>` offers scan and nothing else.
	stdout, stderr, code = runInProcess(t, "__complete", "help", "sca")
	if code != 0 {
		t.Fatalf("__complete help sca exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "scan\t") && !strings.Contains(stdout, "scan\n") {
		t.Errorf("__complete help sca lacks the %q candidate:\n%s\nstderr: %s", "scan", stdout, stderr)
	}
	if strings.Contains(stdout, "copy\t") {
		t.Errorf("__complete help sca leaked the non-matching %q candidate:\n%s", "copy", stdout)
	}
	if !strings.Contains(stdout, ":4") {
		t.Errorf("__complete help sca output lacks the NoFileComp directive line:\n%s", stdout)
	}
}

// TestHelpCompletionEdgeCases pins the two boundaries of the F2
// completion implementation against cobra's BUILTIN behavior (the
// custom ValidArgsFunction is a verbatim port of cobra's
// InitDefaultHelpCmd completion logic - root.Find over the typed
// words, then the resolved topic's available subcommands, prefix
// filtered):
//
//   - a KNOWN LEAF topic (help <scan><TAB>) resolves to scan, which
//     has no subcommands -> NO candidates, NoFileComp - the
//     implementation must not fall back to offering root's commands
//     (no nonsense);
//   - an UNKNOWN topic (help <bogus><TAB>) - cobra's builtin answers
//     with the ROOT command list here too, because the tree's root
//     sets Args: cobra.ArbitraryArgs, which makes root.Find return NO
//     error for an unknown first word (legacyArgs is only consulted
//     when Args == nil). The custom port inherits that exact wart by
//     construction; it is upstream parity, not an N7/T6.2 regression,
//     and the help COMMAND itself still rejects the unknown topic with
//     the typed exit-2 UsageError (pinned in taxonomy_table_test.go).
func TestHelpCompletionEdgeCases(t *testing.T) {
	// candidateLines strips the directive line (":N") and blank lines
	// from __complete stdout, leaving the offered candidates.
	candidateLines := func(stdout string) []string {
		var out []string
		for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}
			out = append(out, line)
		}
		return out
	}

	t.Run("known leaf topic offers no candidates", func(t *testing.T) {
		stdout, stderr, code := runInProcess(t, "__complete", "help", "scan", "")
		if code != 0 {
			t.Fatalf("__complete help scan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if cands := candidateLines(stdout); len(cands) != 0 {
			t.Errorf("__complete help scan \"\" offered candidates %v (scan has no subcommands; must be none)", cands)
		}
		if !strings.Contains(stdout, ":4") {
			t.Errorf("__complete help scan output lacks the NoFileComp directive line:\n%s", stdout)
		}
	})

	t.Run("unknown topic mirrors cobra builtin (root list)", func(t *testing.T) {
		stdout, stderr, code := runInProcess(t, "__complete", "help", "bogus", "")
		if code != 0 {
			t.Fatalf("__complete help bogus exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		// Cobra-builtin parity: the ROOT command list is offered (see
		// the function doc for why). Pin it so a future change to
		// either cobra or the port is a deliberate decision.
		cands := candidateLines(stdout)
		got := map[string]bool{}
		for _, c := range cands {
			got[strings.SplitN(c, "\t", 2)[0]] = true
		}
		for _, name := range append(append([]string(nil), stubCommands...), "help") {
			if !got[name] {
				t.Errorf("__complete help bogus \"\" lacks the %q candidate (cobra builtin parity): %v", name, cands)
			}
		}
		if !strings.Contains(stdout, ":4") {
			t.Errorf("__complete help bogus output lacks the NoFileComp directive line:\n%s", stdout)
		}
		// The CONTRAST that keeps this honest: the help COMMAND still
		// classifies the unknown topic as usage (exit 2) - completion
		// leniency never leaks into command execution.
		if _, _, helpCode := runInProcess(t, "help", "bogus"); helpCode != 2 {
			t.Errorf("help bogus exit = %d, want 2 (completion leniency must not change execution)", helpCode)
		}
	})
}
