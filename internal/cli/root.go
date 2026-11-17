package cli

// T0.5: the cobra root command, the global flags of DESIGN_SPEC 1.1, and
// the execution wrapper that turns a command result into a process exit
// code per DESIGN_SPEC 10.1/10.2. This layer holds NO business logic
// (DESIGN_SPEC 1.2): it parses flags, validates flag combinations,
// invokes exactly one command, classifies the result via report.Classify,
// prints the error to stderr, and exits. Wiring commands to services
// happens in each command's own task (T1.6 and later).
//
// Stream separation (DESIGN_SPEC 10.3, FR-046): stdout carries command
// results only (help included, it is the requested output); every error
// and the not-yet-implemented placeholder go to stderr.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/fsmeta"
	"isopress/internal/report"
)

// rootOptions holds the parsed values of the global persistent flags of
// DESIGN_SPEC 1.1. Later tasks wire them into the service context (catalog
// handle, progress emitter); T0.5 only parses and validates them.
type rootOptions struct {
	catalog     string   // --catalog: path to the SQLite catalog file
	quiet       bool     // --quiet: suppress visual feedback (plain renderer)
	verbose     bool     // --verbose: verbose transcripts on stderr
	exclude     []string // --exclude: repeatable exclusion pattern (scan)
	excludeFrom string   // --exclude-from: file with one pattern per line
	setID       string   // --set-id: explicit backup set identifier
	json        bool     // --json: machine-parseable report on stdout
	strict      bool     // --strict: promote item failures to exit 1
	workDir     string   // --work-dir: staging/temp workspace root
	imageDir    string   // --image-dir: directory holding <SET-ID>-NNN.iso
	// changeDetection is the --change-detection value (global, DESIGN_SPEC
	// 3.5): auto (default, filesystem-class rule) | full | metadata.
	// scan, copy, build AND validate resolve it ONCE per run against
	// their source root (resolveChangeDetection -> fsmeta's statfs
	// derivation); the other commands validate the value eagerly here
	// but never resolve it.
	changeDetection string
}

// NewRootCmd builds the root command with the global persistent flags
// (DESIGN_SPEC 1.1) and the nine subcommands - all nine are real and
// fully wired to their services (T1.6-T5.4; T5.3 removed the last T0.5
// stub). Tests construct roots directly; Execute builds one per run.
func NewRootCmd() *cobra.Command {
	opts := &rootOptions{}
	root := &cobra.Command{
		Use:   "isopress",
		Short: "Backup files and directories to disk or ISO images",
		// Single version source (release process, 2026-11-09):
		// catalog.ToolVersion stamps the media manifests AND feeds
		// cobra's built-in --version flag (default template prints
		// "isopress version <v>"). Cobra registers the flag lazily on
		// execute (InitDefaultVersionFlag), as a LOCAL flag of the
		// command being run - it does not touch the persistent global
		// flag set pinned by TestGlobalFlagsRegistered, and it takes
		// no "-v" shorthand because root's --verbose already owns it
		// (cobra only adds the shorthand when "v" is free).
		Version: catalog.ToolVersion,
		Long: "isopress scans a source tree into a SQLite catalog, plans the " +
			"result onto backup media (copy trees or ISO 9660 images), copies or " +
			"masters them with verify-then-mark ordering, verifies the media, and " +
			"restores from it. Designed for cron/systemd usage: results on stdout, " +
			"progress and errors on stderr, meaningful exit codes." +
			" Exit codes (the scheme; each command's help lists what it can emit): " +
			"0 ok, 1 fatal, 2 usage, 3 missing xorriso or unavailable media, 4 catalog " +
			"error, 5 catalog busy, 10 item failures (--strict: 1), 130 interrupted. " +
			"JSON: --json prints one JSON document on stdout.",
		SilenceErrors: true, // the execute wrapper owns error printing (exit codes, stderr)
		SilenceUsage:  true, // usage is printed only for usage-class errors, by the wrapper

		// The root rejects stray arguments with a typed UsageError
		// (DESIGN_SPEC 10.1) instead of cobra's untyped default, so
		// unknown subcommands classify deterministically to exit 2.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return &report.UsageError{
					Command: cmd.CommandPath(),
					Msg: fmt.Sprintf("unknown command %q for %q (run '%s --help' for the command list)",
						args[0], cmd.CommandPath(), cmd.CommandPath()),
				}
			}
			return cmd.Help()
		},
	}

	// Keep the command surface exactly the design's tree (DESIGN_SPEC
	// 1.1 lists nine commands; cobra's generated "completion" command is
	// not one of them).
	root.CompletionOptions.DisableDefaultCmd = true

	// Custom help command (T6.1 taxonomy sweep; the T0.5 review's
	// deferred NB N7): cobra's BUILT-IN help command prints the root
	// help and exits 0 even for an UNKNOWN topic ("isopress help
	// frobnicate"), while the root's own unknown-subcommand path is a
	// typed UsageError (exit 2, DESIGN_SPEC 10.1) - an inconsistency
	// the taxonomy does not sanction. This replacement keeps every
	// legitimate behavior (no args -> root help; a known command ->
	// that command's help, exit 0) and returns a typed UsageError for
	// an unknown topic, so both "unknown command" spellings classify
	// identically to exit 2.
	//
	// T6.2 addendum (the reviewer's one-liners F3 and F2, same site):
	// the N7 rewrite dropped TWO cobra-builtin behaviors and both are
	// restored below, pinned by help_parity_test.go.
	//
	// Pre-release R1 addendum: cobra's builtin help command also calls
	// InitDefaultVersionFlag (command.go:1305) so the topic path
	// renders the same "--version" flags-block entry the direct path
	// does - without it, `isopress help` and `isopress --help` would
	// diverge byte-for-byte now that the root carries a Version
	// (TestHelpBareTopicCarriesRootFlagsBlock).
	var helpCmd *cobra.Command
	helpCmd = &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		Long: "Help provides help for any command in the application.\n" +
			"Simply type isopress help [path to command] for full details.",
		// F2: cobra's built-in help command completes command names
		// here; the N7 replacement answered with NO candidates. This
		// mirrors the builtin (cobra command.go InitDefaultHelpCmd):
		// resolve the topic the typed words so far name (the root when
		// none), offer its available subcommands - plus the help command
		// itself, exactly as the builtin's `subCmd == cmd.helpCommand`
		// clause does - filtered by the prefix being completed, and
		// never fall back to file completion.
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			target, _, err := root.Find(args)
			if err != nil {
				// Unable to resolve the typed topic (the builtin's
				// answer): no candidates, no file completion.
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			if target == nil {
				target = root // root help topic
			}
			var completions []string
			for _, sub := range target.Commands() {
				if sub.IsAvailableCommand() || sub == helpCmd {
					if strings.HasPrefix(sub.Name(), toComplete) {
						completions = append(completions, cobra.CompletionWithDesc(sub.Name(), sub.Short))
					}
				}
			}
			return completions, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _, err := root.Find(args)
			// root.Find falls back to the root itself when nothing
			// matches, and only errors on ambiguous prefixes - both are
			// "unknown topic" from the user's perspective when args
			// were given.
			if len(args) > 0 && (err != nil || target == root) {
				return &report.UsageError{
					Command: root.CommandPath(),
					Msg: fmt.Sprintf("unknown help topic %q (run '%s --help' for the command list)",
						args[0], root.CommandPath()),
				}
			}
			// F3: the N7 replacement rendered the topic's help WITHOUT
			// the "-h, --help" flags block, because the target never
			// executed and cobra only calls InitDefaultHelpFlag on the
			// command it executes ('help scan' lacked the block while
			// 'scan --help' showed it). The built-in help command calls
			// InitDefaultHelpFlag before Help for exactly this reason;
			// the call is idempotent, so parity costs nothing. The same
			// holds for the version flag (R1): cobra's builtin also
			// calls InitDefaultVersionFlag (command.go:1305), and the
			// root now carries a Version.
			target.InitDefaultHelpFlag()
			target.InitDefaultVersionFlag()
			return target.Help()
		},
	}
	root.SetHelpCommand(helpCmd)

	// Flag-parse failures (unknown flag, bad value) become typed
	// UsageErrors so Classify maps them to exit 2 (DESIGN_SPEC 10.1).
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &report.UsageError{Command: cmd.CommandPath(), Err: err}
	})

	// Global persistent flags, exactly per DESIGN_SPEC 1.1 and
	// IMPLEMENTATION_PLAN T0.5. Types stay minimal for stubs (strings,
	// bools, string slices); real semantics arrive with each command.
	pf := root.PersistentFlags()
	pf.StringVar(&opts.catalog, "catalog", "", "path to the SQLite catalog file")
	pf.BoolVarP(&opts.quiet, "quiet", "q", false, "suppress visual feedback (periodic plain progress lines remain)")
	pf.BoolVarP(&opts.verbose, "verbose", "v", false, "print verbose transcripts on stderr")
	pf.StringArrayVar(&opts.exclude, "exclude", nil, "exclusion pattern (repeatable; scan-time only)")
	pf.StringVar(&opts.excludeFrom, "exclude-from", "", "file with one exclusion pattern per line (# comments, blanks ignored)")
	pf.StringVar(&opts.setID, "set-id", "", "backup set identifier (default: derived from source root and date)")
	pf.BoolVar(&opts.json, "json", false, "emit the final report as JSON on stdout")
	pf.BoolVar(&opts.strict, "strict", false, "promote any item failure to a fatal stop-at-end error (exit 1) (inert for plan, report and clear-assignments: those commands have no per-item failure path)")
	pf.StringVar(&opts.workDir, "work-dir", "", "workspace for staging and temporary files (default: OS temp directory)")
	pf.StringVar(&opts.imageDir, "image-dir", "", "directory containing <SET-ID>-NNN.iso images for restore")
	pf.StringVar(&opts.changeDetection, "change-detection", fsmeta.FlagAuto,
		"change-detection comparison set (scan, copy, build, validate): auto (statfs the source root; CIFS/SMB3 sources degrade to size+mtime), full (force size+mtime+dev+inode), metadata (force size+mtime)")

	// Minimal flag-combination validation. --quiet and --verbose are NOT
	// exclusive: DESIGN_SPEC 9.2 treats both as plain-renderer inputs, so
	// combining them is allowed and no exclusivity check exists here.
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		return validateGlobalFlags(cmd, opts)
	}

	for _, sub := range newCommands(opts) {
		root.AddCommand(sub)
	}
	return root
}

// validateGlobalFlags enforces the flag combinations the spec actually
// constrains. --exclude-from must name an existing readable path when
// provided (DESIGN_SPEC 3.2 input; a missing file is a usage error,
// 10.1). Every PATH-typed global flag (--catalog, --exclude-from,
// --work-dir, --image-dir) is additionally rejected when its value is a
// leading-tilde shell token that quoting kept literal (checkTilde,
// paths.go; PM decision D1 of 2026-10-19) - a usage error naming the
// flag, before any command touches the filesystem.
// --change-detection (DESIGN_SPEC 3.5, added with the 2026-10-20 CIFS
// amendment) must be one of auto|full|metadata - same eager usage-error
// discipline. Nothing else is constrained at T0.5.
func validateGlobalFlags(cmd *cobra.Command, opts *rootOptions) error {
	for _, pv := range []struct{ name, value string }{
		{"--catalog", opts.catalog},
		{"--exclude-from", opts.excludeFrom},
		{"--work-dir", opts.workDir},
		{"--image-dir", opts.imageDir},
	} {
		if err := checkTilde(pv.value, pv.name); err != nil {
			return &report.UsageError{Command: cmd.CommandPath(), Err: err}
		}
	}
	if opts.excludeFrom != "" {
		if _, err := os.Stat(opts.excludeFrom); err != nil {
			return &report.UsageError{
				Command: cmd.CommandPath(),
				Err: fmt.Errorf("--exclude-from %q cannot be read: %w",
					opts.excludeFrom, err),
			}
		}
	}
	// --change-detection accepts exactly auto|full|metadata (DESIGN_SPEC
	// 3.5); anything else is a usage error (exit 2, 10.1) - validated
	// EAGERLY here, before any command touches the filesystem, like the
	// tilde and --exclude-from gates above. The consuming commands
	// (scan/copy/build/validate) additionally wrap fsmeta's sentinel
	// through resolveChangeDetection for defense in depth.
	switch opts.changeDetection {
	case fsmeta.FlagAuto, fsmeta.FlagFull, fsmeta.FlagMetadata:
	default:
		return &report.UsageError{
			Command: cmd.CommandPath(),
			Err: fmt.Errorf("--change-detection %q must be one of %s, %s, %s",
				opts.changeDetection, fsmeta.FlagAuto, fsmeta.FlagFull, fsmeta.FlagMetadata),
		}
	}
	return nil
}

// Execute is the process entry point used by main. It installs the
// SIGINT/SIGTERM scaffolding of DESIGN_SPEC 10.1 (Interrupted class,
// exit 130), runs the root command, prints any error to stderr, and
// returns the exit code from the 10.2 scheme.
//
// Signal scaffolding (defect B, review round 2 of 2026-10-19): ONE
// registered os/signal channel, ONE consumer goroutine. The first
// SIGINT/SIGTERM cancels the context (the graceful path: an in-flight
// command aborts at its next context check instead of dying mid-write;
// the default disposition stays suppressed meanwhile), and a SECOND
// signal during the subsequent shutdown resets the default disposition
// and re-raises to self so the kernel can still terminate a command
// stuck in a long uninterruptible stretch (the hard escape - only
// SIGKILL could escape before it). The FIRST signal's exit-130 mapping
// is untouched (see execute); the escape only acts after the first
// signal has already been consumed. armSecondSignalEscape owns the
// registration; signal.NotifyContext is deliberately NOT used - its
// private delivery channel was half of the defect-B race (see arm's
// doc).
func Execute() int {
	ctx, cancel := context.WithCancel(context.Background())
	// cancel is idempotent: the guard goroutine calls it on the FIRST
	// signal; this defer is the safety net for the no-signal path.
	defer cancel()
	// done closes at Execute exit - the FIRST defer to run in the LIFO
	// exit order (close, then cancel), so the guard goroutine always
	// observes retirement on every path: clean no-signal run (phase 1)
	// and normal single-signal shutdown (phase 2, waiting for a second
	// signal that never comes) alike - no goroutine leak in the 99.9%
	// cases.
	done := make(chan struct{})
	defer close(done)
	armSecondSignalEscape(cancel, done, os.Stderr)
	return execute(ctx, NewRootCmd(), os.Args[1:], os.Stderr)
}

// armSecondSignalEscape wires the production signal guard: it registers
// ONE os/signal channel for SIGINT/SIGTERM SYNCHRONOUSLY - before the
// goroutine starts and long before the first command runs (signal.Notify
// is fully effective when it returns, so once arm returns there is no
// registration window in which a fast ^C^C could slip its second press
// past the escape) - and starts the single consumer goroutine via the
// injectable startSecondSignalEscape core (stderr for the forcing line,
// resetAndReraise as the kill action).
//
// Why ONE channel (defect B root cause, review round 2): the previous
// design split every signal's delivery across TWO registered channels -
// signal.NotifyContext's private one plus the escape's - and compensated
// with a "drain exactly one copy" step. That drain's premise is FALSE on
// Go 1.26: os/signal's process() hands copies to registered channels via
// MAP ITERATION with randomized order, so the escape goroutine could
// observe the cancellation BEFORE the first signal's copy landed in its
// own channel, drain nothing, and then misread the first signal's real
// copy as a SECOND signal - forcing exit on a SINGLE ^C (the process
// then dies by default action: exit 143 for SIGTERM, not 130, skipping
// graceful shutdown entirely; reproduced live). With a single channel
// consumed in FIFO order by a single goroutine, first-vs-second is a
// pure ORDERING question and no drain exists to get wrong.
func armSecondSignalEscape(cancel context.CancelFunc, done <-chan struct{}, stderr io.Writer) {
	// Buffer 2 tolerates a first and a second signal both landing before
	// the goroutine wakes.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	startSecondSignalEscape(sigs, cancel, done, stderr, resetAndReraise)
}

// startSecondSignalEscape is the injectable core of the signal guard: ONE
// goroutine consuming ONE channel, in two phases. Phase 1 waits for the
// FIRST signal on sigs and then calls cancel - the graceful path (the
// command aborts at its next context check; execute's exit-130 mapping
// for a signal-cancelled context is untouched). Phase 2 is the hard
// escape: the NEXT signal on the channel prints the forcing-exit line to
// stderr and invokes force with the received signal (production force,
// resetAndReraise, never returns - it kills this process; tests inject a
// recorder).
//
// Single-channel FIFO makes first-vs-second exact: the goroutine
// consumes the first signal's copy from the channel BEFORE entering the
// phase-2 select, so a genuine second signal can never be mistaken for
// the first one's copy (the cross-channel drain race this replaces is
// gone by construction - there is no second channel and no drain).
//
// Goroutine lifecycle (no-leak guarantee, test-pinned): the goroutine
// returns when done closes, whichever phase it is in, and deregisters
// the channel on the way out (defer signal.Stop - the reviewed
// retirement shape; every retirement path passes through it, and
// Execute's LIFO exit order guarantees done closes first). When the
// injected force RETURNS (production dies in force instead), the
// goroutine retires too: the escape fires exactly once per run and
// further signals on the channel are inert.
//
// Delivery corner (documented, inherent to Unix standard signals, the
// ONLY remaining loss mode): standard signals do not queue - duplicates
// raised within the same kernel delivery window MERGE into one delivery.
// Because everything is FIFO on one channel this can no longer eat the
// second signal the way the old drain could: the first signal is
// consumed from the channel before the phase-2 select is even entered,
// so any later delivery - merged or not - is treated as the escape
// trigger by position, never misattributed. The worst remaining case is
// a duplicate that merges with its immediate predecessor (a hyper-rapid
// ^C^C^C presenting as two deliveries): the escape still fires, exactly
// once, on the surviving delivery; no userspace code can observe what
// the kernel merged away.
func startSecondSignalEscape(sigs chan os.Signal, cancel context.CancelFunc, done <-chan struct{}, stderr io.Writer, force func(os.Signal)) {
	go func() {
		defer signal.Stop(sigs)
		// Phase 1: the FIRST signal (graceful path), or Execute
		// finishing with no signal at all.
		select {
		case <-sigs:
			cancel() // first signal observed: graceful shutdown begins
		case <-done:
			return
		}
		// Phase 2: the hard escape. The first signal's copy is already
		// consumed above (channel FIFO), so whatever arrives here is a
		// genuine post-first signal.
		select {
		case sig := <-sigs:
			fmt.Fprintln(stderr, "isopress: second signal received, forcing exit") //nolint:errcheck // stderr is best-effort: this line runs inside the second-signal escape, where nothing can change the outcome anymore
			force(sig)                                                             // production: resetAndReraise - the process dies here
		case <-done: // shutdown finished before a second signal arrived
		}
	}()
}

// resetAndReraise restores the DEFAULT disposition for sig and re-raises
// it to this process, so the kernel delivers the default action (exit for
// SIGINT/SIGTERM) exactly as if the signal had never been caught. It
// returns only if the kill itself fails (impossible for the signals we
// register); callers treat a return as "escape failed, keep running".
func resetAndReraise(sig os.Signal) {
	sysSig, ok := sig.(syscall.Signal)
	if !ok {
		// Cannot happen for the registered set (os.Interrupt and
		// syscall.SIGTERM are both syscall.Signal); degrade to the
		// documented default rather than panic in a kill path.
		sysSig = syscall.SIGTERM
	}
	signal.Reset(sysSig)                       // SIG_DFL restored for this signal
	_ = syscall.Kill(syscall.Getpid(), sysSig) // re-raise: the default action fires
}

// execute runs root over args with the given context and error writer,
// returning the DESIGN_SPEC 10.2 exit code. It is the injectable seam
// behind Execute: tests pass a buffer as stderr (and a scripted context)
// to assert codes and stream separation deterministically.
//
// Error handling (DESIGN_SPEC 10.1, 10.3):
//   - the command's error is classified with report.Classify;
//   - a signal that cancelled the context marks the run Interrupted
//     (exit 130) only when nothing MORE SPECIFIC classifies first.
//     Classify checks the typed classes - usage (2), dependency (3),
//     catalog busy (5), catalog error (4), item failures (10) -
//     BEFORE ErrInterrupted, so a command result that carries one of
//     those classes keeps its own exit code even when the run was also
//     cancelled (a catalog failure at the first transaction statement
//     of a cancelled run exits 4; item failures collected before the
//     signal exit 10). Exit 130 applies when the result classifies as
//     interrupted: a nil result, a plain unclassified error, or an
//     error that already carries the Interrupted class - all three are
//     folded into the Interrupted class here;
//   - errors print to stderr ONLY - stdout stays reserved for command
//     results (FR-046);
//   - usage-class errors carry a usage hint (10.1);
//   - a fatal-unexpected error (the bug class, exit 1) prints a
//     goroutine stack dump to stderr under --verbose (10.1/10.2: "stack
//     trace under --verbose"). Go's error values carry no origin stack,
//     so the honest dump is the set of live goroutines at exit -
//     enough to locate a wedged or panicking code path.
func execute(ctx context.Context, root *cobra.Command, args []string, stderr io.Writer) int {
	root.SetArgs(args)
	root.SetErr(stderr)

	err := root.ExecuteContext(ctx)
	// A signal that arrived during the run cancels the context. Fold
	// the command's result into the Interrupted class so it exits 130
	// below: that fold is what carries a nil result, a plain
	// unclassified error, or an already-interrupted one. A result that
	// carries a MORE SPECIFIC typed class (usage, dependency, catalog,
	// item failures) keeps that class instead - Classify checks them
	// before ErrInterrupted, and isInterrupted below defers to them.
	//
	// T6.1, the T0.5 review's deferred NB N1: the ORIGINAL error chain is
	// PRESERVED by wrapping it alongside ErrInterrupted (double-%w), so
	// the stderr output names what was actually interrupted (the walk
	// error, the catalog fault, ...) instead of the old override that
	// discarded the cause entirely. The message keeps the historical
	// "signal received, aborting" clause so existing stderr pins hold.
	if ctx.Err() != nil && !isInterrupted(err) {
		switch {
		case err != nil:
			err = fmt.Errorf("%w: signal received, aborting: %w", report.ErrInterrupted, err)
		default:
			err = fmt.Errorf("%w: signal received, aborting", report.ErrInterrupted)
		}
	}
	if err == nil {
		return report.ExitOK
	}

	code := report.Classify(err)
	fmt.Fprintf(stderr, "isopress: error: %v\n", err) //nolint:errcheck // stderr is best-effort: the returned exit code is the contract, a failed print (closed pipe) must not change it
	if code == report.ExitUsage {
		fmt.Fprintln(stderr, "run 'isopress --help' or 'isopress <command> --help' for usage") //nolint:errcheck // stderr is best-effort: a usage hint that cannot be printed never changes the usage exit code
	}
	// DESIGN_SPEC 10.2: the fatal-unexpected class (exit 1) prints a
	// stack trace under --verbose. Only the bug class gets it: the
	// typed classes (2/3/4/5/10/130) are diagnosed by their messages
	// and a goroutine dump would be noise. The flag is read from the
	// parsed persistent flag set, so a minimal test root without the
	// flag simply never dumps.
	if code == report.ExitFatal && verboseFlagSet(root) {
		stack := make([]byte, 64<<10) // 64 KiB grows to the full dump
		stack = stack[:runtime.Stack(stack, true)]
		fmt.Fprintf(stderr, "isopress: fatal unexpected error: goroutine stack dump (--verbose):\n%s\n", stack) //nolint:errcheck // stderr is best-effort: the stack dump is diagnostic only and must never move the exit code
	}
	return code
}

// verboseFlagSet reports whether the root's parsed --verbose persistent
// flag is set. It tolerates a root built without the flag (tests
// construct minimal roots), never panicking on a missing lookup.
func verboseFlagSet(root *cobra.Command) bool {
	if root.PersistentFlags().Lookup("verbose") == nil {
		return false
	}
	verbose, err := root.PersistentFlags().GetBool("verbose")
	return err == nil && verbose
}

// isInterrupted reports whether err already carries the Interrupted
// class, so the context-cancel override never masks a more specific
// classification the command itself produced.
func isInterrupted(err error) bool {
	return err != nil && report.Classify(err) == report.ExitInterrupted
}
