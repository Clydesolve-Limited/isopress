package cli

// T6.2 acceptance: the --json contract of DESIGN_SPEC 9.3 ("stdout
// carries only command results and reports, making piping
// machine-parseable; --json puts the final report on stdout in JSON
// form"), 10.3 (log and progress separation guarantees) and PLAN_SPEC
// FR-046 / NFR-003, locked as a golden-schema contract over EVERY
// reporting command: scan, plan, report, clear-assignments, copy,
// build, verify-media, validate, restore.
//
// MECHANISM: the shipped frozen-key style, extended with one shared
// helper for this suite (the brief's "small shared helper in the test
// package") - the existing pins are ad-hoc per command (scan's
// jsonReportKeysClean + sortedKeys/sameStrings, planreport's shared
// slice, the inline want lists of copy/build/verify-media/validate/
// restore, assertFrozenJSONKeys):
//
//   - goldenJSONDoc: EXACTLY ONE JSON document on stdout (a single
//     encoding/json Decode, then io.EOF - nothing before, between, or
//     after it) + the frozen top-level key set;
//   - assertJSONTypes / assertArrayElementTypes: the JSON VALUE TYPE
//     of every pinned key (string/number/bool/array/object), so a key
//     that flips to null or changes scalar class fails even when the
//     key set is unchanged;
//   - assertJSONVariance: the per-run variance fields (run ids, paths,
//     per-image ids) asserted by SHAPE, never by literal: run_id is a
//     number >= its floor, path fields are absolute strings, image
//     ids are positive numbers;
//   - assertJSONModeStreams: 10.3 end to end through the capturing
//     renderer (every runner below captures both process streams, and
//     stderr - not stdout - is the TTY-detection input of 9.2, so the
//     plain renderer renders into the captured stderr): each human
//     progress/diagnostic marker MUST be on stderr and MUST NOT be on
//     stdout, warnings stay on stderr even in --json mode (the
//     T5.5-era behavior), and stdout is exactly the one document.
//
// STATES. Each command covers the states the design has (the brief's
// SUCCESS / EMPTY / PARTIAL acceptance):
//
//	success  - the run did work, status ok;
//	empty    - nothing to do, exit 0 with the "nothing" flags
//	           (plan nothing_to_plan, report nothing_to_plan, clear
//	           nothing_to_clear, scan of an empty root, copy's
//	           steady-state resume, validate's zero-record set);
//	partial  - the exit-10 shape: status partial and the per-item
//	           listing (failures / findings / problems) carries the
//	           failures, enumerated.
//
// Where the design has NO such state, the cell says so and pins the
// shipped substitute instead: plan/report/clear are transactional (no
// item-failure class exists - their --strict inertness is pinned in
// plan_e2e_test.go / reportcmd_test.go), and build/verify-media/
// restore refuse an empty job with the usage-class "no planned images"
// / selector gate - pinned here as "no document on stdout under
// --json" (10.3: abort-before-work classes print nothing, and --json
// never fabricates an empty report).
//
// The frozen key sets themselves are NOT changed by this task: every
// slice below matches shipped behavior; a contradiction would have
// stopped the task (see WORK_LOG's T6.2 entry - none found).
//
// TestJSONModeExitCodesMatchHumanMode is the exit-code half: --json
// never changes a run's exit code (spot-check, every command).

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"isopress/internal/report"
)

// --- shared helpers -------------------------------------------------------------

// jsonKindOf classifies a decoded JSON value the way the golden
// schema records it. A nil interface is JSON null - never an accepted
// kind below, so an omitempty hole in a pinned key fails the type pin.
func jsonKindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "bool"
	case float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// assertJSONTypes pins the JSON value type of each named key of doc.
func assertJSONTypes(t *testing.T, doc map[string]any, want map[string]string) {
	t.Helper()
	for key, wantKind := range want {
		v, ok := doc[key]
		if !ok {
			t.Errorf("key %q missing from the document (want kind %s)", key, wantKind)
			continue
		}
		if got := jsonKindOf(v); got != wantKind {
			t.Errorf("key %q has kind %s, want %s (value: %#v)", key, got, wantKind, v)
		}
	}
}

// assertArrayElementTypes pins the value types of the given keys of
// EVERY object element of the named array key (the per-item listings:
// images, destinations, findings, failures).
func assertArrayElementTypes(t *testing.T, doc map[string]any, arrayKey string, want map[string]string) {
	t.Helper()
	arr, ok := doc[arrayKey].([]any)
	if !ok {
		t.Errorf("%q is %T, want an array", arrayKey, doc[arrayKey])
		return
	}
	for i, elem := range arr {
		m, ok := elem.(map[string]any)
		if !ok {
			t.Errorf("%s[%d] is %T, want an object", arrayKey, i, elem)
			continue
		}
		for key, wantKind := range want {
			v, ok := m[key]
			if !ok {
				t.Errorf("%s[%d] key %q missing (want kind %s)", arrayKey, i, key, wantKind)
				continue
			}
			if got := jsonKindOf(v); got != wantKind {
				t.Errorf("%s[%d] key %q has kind %s, want %s (value: %#v)", arrayKey, i, key, got, wantKind, v)
			}
		}
	}
}

// goldenJSONDoc decodes EXACTLY ONE JSON document from stdout (the
// piping guarantee: one Decode, then EOF - T6.2's stdout half of 10.3)
// and asserts the frozen top-level key set (the shipped schema-freeze
// style). The returned doc feeds the type/variance/status pins.
func goldenJSONDoc(t *testing.T, stdout string, wantKeys []string) map[string]any {
	t.Helper()
	doc := decodeSingleJSONDoc(t, stdout)
	want := append([]string(nil), wantKeys...)
	sort.Strings(want)
	if got := sortedKeys(doc); !sameStrings(got, want) {
		t.Errorf("--json key set = %v\nwant exactly %v (schema freeze: changing this must be deliberate)",
			got, want)
	}
	return doc
}

// assertJSONModeStreams is the 10.3 stream contract for ONE --json run
// through the capturing renderer: every human marker (progress, Done
// summary, warning, error line) must be CARRIED BY STDERR and must be
// ABSENT FROM STDOUT. Together with goldenJSONDoc's single-document
// decode this pins "stdout = the one JSON document and nothing else,
// stderr = the human channel" per command and per state.
//
// This is deliberately END-TO-END (real command runs over the real
// tree), the complement of T6.1's AST grep in
// stream_separation_test.go, which covers direct writes from
// service/catalog code - not command runs.
func assertJSONModeStreams(t *testing.T, stdout, stderr string, markers ...string) {
	t.Helper()
	// The two diagnostic classes that must NEVER ride stdout in any
	// mode (FR-046/10.3) - asserted unconditionally for every cell.
	for _, forbidden := range []string{"warning:", "isopress: error:"} {
		if strings.Contains(stdout, forbidden) {
			t.Errorf("stdout carries the diagnostic %q in --json mode (diagnostics belong on stderr):\n%s",
				forbidden, stdout)
		}
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("stderr is empty: the human progress/diagnostics channel must carry the run's output (10.3)")
	}
	for _, marker := range markers {
		if !strings.Contains(stderr, marker) {
			t.Errorf("stderr lacks the human marker %q:\n%s", marker, stderr)
		}
		if strings.Contains(stdout, marker) {
			t.Errorf("human marker %q leaked onto stdout in --json mode:\n%s", marker, stdout)
		}
	}
}

// assertRunIDShape pins a run-id field by shape: present, numeric, at
// least floor (0 is the documented no-op floor: clear's
// nothing-to-clear report mints no run row and reports run_id 0).
func assertRunIDShape(t *testing.T, doc map[string]any, floor float64) {
	t.Helper()
	v, ok := doc["run_id"].(float64)
	if !ok {
		t.Errorf("run_id = %#v, want a number", doc["run_id"])
		return
	}
	if v < floor {
		t.Errorf("run_id = %v, want >= %v", v, floor)
	}
}

// assertAbsPathShape pins a path-carrying field by shape: a non-empty
// absolute path (per-run variance - the literal lives in a temp dir).
func assertAbsPathShape(t *testing.T, doc map[string]any, key string) {
	t.Helper()
	v, ok := doc[key].(string)
	if !ok || v == "" {
		t.Errorf("%s = %#v, want a non-empty string path", key, doc[key])
		return
	}
	if !filepath.IsAbs(v) {
		t.Errorf("%s = %q, want an absolute path", key, v)
	}
}

// assertImageEntriesShape pins the per-image variance fields by SHAPE
// (never by literal): every images[] entry carries a POSITIVE image id
// and, where the schema has one, an absolute media path.
func assertImageEntriesShape(t *testing.T, doc map[string]any, withPath bool) {
	t.Helper()
	arr, ok := doc["images"].([]any)
	if !ok {
		t.Errorf("images = %#v, want an array", doc["images"])
		return
	}
	for i, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			t.Errorf("images[%d] = %#v, want an object", i, e)
			continue
		}
		if id, ok := m["image"].(float64); !ok || id < 1 {
			t.Errorf("images[%d].image = %#v, want a positive image number", i, m["image"])
		}
		if withPath {
			p, ok := m["path"].(string)
			if !ok || p == "" || !filepath.IsAbs(p) {
				t.Errorf("images[%d].path = %#v, want an absolute path", i, m["path"])
			}
		}
	}
}

// assertJSONStdoutBytesClean pins the BYTE hygiene of a --json stdout
// stream (the "no stray bytes" audit of the T6.2 review): the stream
// must START with the opening brace and END with the closing brace
// followed by EXACTLY ONE newline (json.Encoder's "}\n"). A UTF-8 BOM
// or any leading/trailing byte that encoding/json.Decode would
// tolerate (leading whitespace, a second trailing newline, a trailing
// space) is still a hazard for naive byte-level consumers (jq on a
// pipe, fixed-offset scripts) and hides stray writes, so it is pinned
// OUT of the contract here, per command.
func assertJSONStdoutBytesClean(t *testing.T, stdout string) {
	t.Helper()
	if strings.HasPrefix(stdout, "\ufeff") {
		t.Errorf("stdout starts with a UTF-8 BOM (breaks naive byte-level consumers): %q",
			[]byte(stdout[:min(len(stdout), 8)]))
	}
	if !strings.HasPrefix(stdout, "{") {
		head := stdout
		if len(head) > 40 {
			head = head[:40]
		}
		t.Errorf("stdout does not start with the opening brace (stray bytes BEFORE the document): %q", head)
	}
	// "}\n" as the exact tail: a second trailing newline, a trailing
	// space/tab/CR, or trailing garbage all fail this check even when
	// they still decode.
	if !strings.HasSuffix(stdout, "}\n") {
		tail := stdout
		if len(tail) > 40 {
			tail = tail[len(tail)-40:]
		}
		t.Errorf("stdout must end with the closing brace + exactly one newline; tail = %q", tail)
	}
}

// extractNums pulls the capture groups of re out of s as ints (the
// cross-leg WORK-EVIDENCE comparisons of the exit-parity table: the
// human presentation's summary line vs the JSON document's counts). A
// missing match or a non-int capture is a hard failure - a summary
// format drift must surface here, not silently weaken the parity
// claim. When last is true the LAST match wins (progress lines share
// prefixes with the final Done summary; the summary is emitted last).
func extractNums(t *testing.T, what, re string, s string, last bool) []int {
	t.Helper()
	ms := regexp.MustCompile(re).FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		t.Fatalf("no %s match for %q in:\n%s", what, re, s)
	}
	m := ms[0]
	if last {
		m = ms[len(ms)-1]
	}
	out := make([]int, len(m)-1)
	for i, g := range m[1:] {
		n, err := strconv.Atoi(g)
		if err != nil {
			t.Fatalf("%s capture %q is not an int: %v", what, g, err)
		}
		out[i] = n
	}
	return out
}

// jsonNum reads a numeric field of a decoded document as an int (the
// JSON half of the cross-leg comparisons).
func jsonNum(t *testing.T, doc map[string]any, key string) int {
	t.Helper()
	v, ok := doc[key].(float64)
	if !ok {
		t.Fatalf("%s = %#v, want a number", key, doc[key])
	}
	return int(v)
}

// --- frozen key sets (consolidated; companion pins live in each command's suite) ---

// copyJSONKeys is the frozen top-level key set of the copy --json
// document (13 keys; "failures" is omitempty and joins on runs with
// failures - pinned in both shapes below, mirroring copy_test.go's
// inline pin; "wipes" is the ratified 4.6.1 wipe-gate addition, ALWAYS
// present and [] when empty - the deliberate 12 -> 13 move).
var copyJSONKeys = []string{
	"already_copied", "bytes", "copied", "dest", "dirs", "failed",
	"images", "metadata_warnings", "run_id", "set_id", "skipped", "status",
	"wipes",
}

// buildJSONKeys is the frozen top-level key set of the build --json
// document (10 keys; "failures" omitempty - build_test.go pin).
var buildJSONKeys = []string{
	"built", "bytes", "dest", "failed", "images", "run_id", "set_id",
	"status", "verified_entries", "warnings",
}

// verifyMediaJSONKeys is the frozen top-level key set of the
// verify-media --json document (9 keys - verifymedia_test.go pin).
var verifyMediaJSONKeys = []string{
	"images", "matched", "mismatched", "missing", "set_id", "status",
	"unexpected", "unreadable", "verified",
}

// validateJSONKeys is the frozen top-level key set of the validate
// --json document (13 keys - validate_test.go pin; "files" is NEVER
// omitempty: empty findings encode as []).
var validateJSONKeys = []string{
	"anomalies", "broken", "changed", "comparison_set", "corrupt",
	"files", "missing", "new", "ok", "records", "root", "set_id", "status",
}

// restoreJSONKeys is the frozen top-level key set of the restore
// --json document (11 keys - restore_test.go pin; "unclaimed" is
// never nil: empty encodes as []).
var restoreJSONKeys = []string{
	"set_id", "target", "selection", "status",
	"files", "bytes", "restored", "skipped", "failed",
	"unclaimed", "images",
}

// --- shared per-command value-type maps (state-independent keys) ------------------

var (
	scanJSONTypes = map[string]string{
		"root": "string", "set_id": "string", "kind": "string", "status": "string",
		"run_id": "number", "scanned": "number", "regular": "number", "symlinks": "number",
		"added": "number", "modified": "number", "unchanged": "number", "missing": "number",
		"unreadable": "number", "changed": "number", "dirs": "number", "bytes": "number",
	}
	planReportJSONTypes = map[string]string{
		"mode": "string", "set_id": "string", "media_profile": "string",
		"nominal_size": "number", "usable_size": "number", "margin_percent": "number",
		"media_count": "number", "new_destinations": "number",
		"assigned_files": "number", "assigned_chunks": "number",
		"assigned_symlinks": "number", "assigned_dirs": "number",
		"secondaries_excluded": "number", "residual_bytes": "number",
		"residual_images": "number", "rechunked_files": "number",
		"rehashed_at_next_write": "number",
		"nothing_to_plan":        "bool", "destinations": "array",
	}
	clearJSONTypes = map[string]string{
		"mode": "string", "set_id": "string", "run_id": "number",
		"files_cleared": "number", "chunks_cleared": "number", "dirs_cleared": "number",
		"destinations_removed": "number", "chunk_hashes_retained": "number",
		"unlocked_images":  "array", // DESIGN 4.6 / M6: sorted []int, present in BOTH modes (9 -> 10 keys)
		"nothing_to_clear": "bool",
	}
	copyJSONTypes = map[string]string{
		"set_id": "string", "dest": "string", "status": "string", "run_id": "number",
		"copied": "number", "already_copied": "number", "skipped": "number",
		"failed": "number", "dirs": "number", "bytes": "number",
		"metadata_warnings": "number", "images": "array",
		"wipes": "array", // DESIGN 4.6.1 channel 2: always present ([] when empty), 12 -> 13 keys
	}
	buildJSONTypes = map[string]string{
		"set_id": "string", "dest": "string", "status": "string", "run_id": "number",
		"built": "number", "failed": "number", "bytes": "number",
		"verified_entries": "number", "warnings": "number", "images": "array",
	}
	verifyMediaJSONTypes = map[string]string{
		"set_id": "string", "status": "string",
		"matched": "number", "mismatched": "number", "missing": "number",
		"unexpected": "number", "unreadable": "number", "verified": "number",
		"images": "array",
	}
	validateJSONTypes = map[string]string{
		"set_id": "string", "root": "string", "status": "string",
		"comparison_set": "string",
		"records":        "number", "ok": "number", "changed": "number", "corrupt": "number",
		"broken": "number", "missing": "number", "new": "number", "anomalies": "number",
		"files": "array",
	}
	restoreJSONTypes = map[string]string{
		"set_id": "string", "target": "string", "selection": "string", "status": "string",
		"files": "number", "bytes": "number", "restored": "number",
		"skipped": "number", "failed": "number",
		"unclaimed": "array", "images": "array",
	}
)

// --- scan ------------------------------------------------------------------------

// TestGoldenJSONSchemaScan covers scan's three states: SUCCESS (fixture
// scanned), EMPTY (an empty root - nothing to walk), PARTIAL (an
// unreadable source, exit 10 with the enumerated failure).
func TestGoldenJSONSchemaScan(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		root := makeFixture(t)
		cat := createCatalog(t)
		args := []string{"--catalog", cat, "--set-id", "S-JSC", "--json"}
		for _, p := range fixtureExcludes() {
			args = append(args, "--exclude", p)
		}
		stdout, stderr, code := runScanChild(t, append(args, root)...)
		if code != report.ExitOK {
			t.Fatalf("scan --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, jsonReportKeysClean)
		assertJSONTypes(t, doc, scanJSONTypes)
		if doc["status"] != "ok" || doc["kind"] != "ingest" || doc["set_id"] != "S-JSC" {
			t.Errorf("identity = %v/%v/%v, want ok/ingest/S-JSC", doc["status"], doc["kind"], doc["set_id"])
		}
		if doc["scanned"] != float64(4) {
			t.Errorf("scanned = %v, want 4 (the fixture)", doc["scanned"])
		}
		if doc["failures"] != nil {
			t.Errorf("clean-run failures = %v, want absent (omitempty)", doc["failures"])
		}
		assertRunIDShape(t, doc, 1)
		assertAbsPathShape(t, doc, "root")
		assertJSONModeStreams(t, stdout, stderr, "scan(hash): files", "scan(hash): files 4")
	})

	t.Run("empty", func(t *testing.T) {
		root := t.TempDir() // nothing to walk: the EMPTY state
		cat := createCatalog(t)
		stdout, stderr, code := runScanChild(t,
			"--catalog", cat, "--set-id", "S-JSE", "--json", root)
		if code != report.ExitOK {
			t.Fatalf("empty scan --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, jsonReportKeysClean)
		assertJSONTypes(t, doc, scanJSONTypes)
		if doc["status"] != "ok" || doc["scanned"] != float64(0) || doc["bytes"] != float64(0) {
			t.Errorf("empty report = status %v scanned %v bytes %v, want ok/0/0",
				doc["status"], doc["scanned"], doc["bytes"])
		}
		assertRunIDShape(t, doc, 1)
		assertAbsPathShape(t, doc, "root")
		assertJSONModeStreams(t, stdout, stderr, "scan(hash): files 0")
	})

	if asRoot() {
		t.Log("scan/partial skipped as root: chmod 000 does not bind root")
		return
	}
	t.Run("partial", func(t *testing.T) {
		root := makeFixture(t)
		locked := filepath.Join(root, "locked.dat")
		if err := os.WriteFile(locked, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
		cat := createCatalog(t)
		args := []string{"--catalog", cat, "--set-id", "S-JSP", "--json"}
		for _, p := range fixtureExcludes() {
			args = append(args, "--exclude", p)
		}
		stdout, stderr, code := runScanChild(t, append(args, root)...)
		if code != report.ExitItemFailures {
			t.Fatalf("partial scan --json exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		wantKeys := append(append([]string(nil), jsonReportKeysClean...), "failures")
		doc := goldenJSONDoc(t, stdout, wantKeys)
		assertJSONTypes(t, doc, scanJSONTypes)
		assertJSONTypes(t, doc, map[string]string{"failures": "array"})
		if doc["status"] != "partial" {
			t.Errorf("status = %v, want partial", doc["status"])
		}
		failures, _ := doc["failures"].([]any)
		if len(failures) != 1 {
			t.Fatalf("failures = %v, want exactly one enumerated failure", doc["failures"])
		}
		line, _ := failures[0].(string)
		if !strings.Contains(line, "locked.dat") || !strings.Contains(line, "unreadable") {
			t.Errorf("failure line %q does not name the unreadable item", line)
		}
		assertRunIDShape(t, doc, 1)
		assertAbsPathShape(t, doc, "root")
		// The T5.5 warning leg: the walker warning rides STDERR only.
		assertJSONModeStreams(t, stdout, stderr,
			"scan(hash): files", "warning: unreadable file locked.dat", "isopress: error: completed with 1 item failure")
	})
}

// --- plan ------------------------------------------------------------------------

// TestGoldenJSONSchemaPlan covers plan's SUCCESS (fresh assignments)
// and EMPTY (re-plan: nothing_to_plan). PARTIAL is design-absent:
// plan is one transaction (4.5) with no per-item failure class - its
// --strict inertness is pinned in plan_e2e_test.go.
func TestGoldenJSONSchemaPlan(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-JPLAN")

	t.Run("success", func(t *testing.T) {
		stdout, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", "S-JPLAN", "--size", planFixtureSize, "--json")
		if code != report.ExitOK {
			t.Fatalf("plan --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, planReportJSONKeys)
		assertJSONTypes(t, doc, planReportJSONTypes)
		assertArrayElementTypes(t, doc, "destinations", map[string]string{
			"image": "number", "media_profile": "string", "nominal_size": "number",
			"usable_size": "number", "used_bytes": "number", "fill_percent": "number",
			"items": "number", "dirs": "number", "locked": "bool",
		})
		if doc["mode"] != "plan" || doc["set_id"] != "S-JPLAN" {
			t.Errorf("identity = %v/%v, want plan/S-JPLAN", doc["mode"], doc["set_id"])
		}
		if doc["nothing_to_plan"] != false {
			t.Errorf("nothing_to_plan = %v, want false (the first plan has work)", doc["nothing_to_plan"])
		}
		if doc["media_count"].(float64) < 1 || doc["assigned_files"].(float64) < 1 {
			t.Errorf("media/assigned = %v/%v, want a real assignment", doc["media_count"], doc["assigned_files"])
		}
		assertJSONModeStreams(t, stdout, stderr, "plan: media")
	})

	t.Run("empty", func(t *testing.T) {
		stdout, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", "S-JPLAN", "--size", planFixtureSize, "--json")
		if code != report.ExitOK {
			t.Fatalf("empty plan --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, planReportJSONKeys)
		assertJSONTypes(t, doc, planReportJSONTypes)
		if doc["nothing_to_plan"] != true {
			t.Errorf("nothing_to_plan = %v, want true (nothing assigned)", doc["nothing_to_plan"])
		}
		if doc["assigned_files"].(float64) != 0 || doc["new_destinations"].(float64) != 0 {
			t.Errorf("empty plan assignments = %v files / %v new, want 0/0",
				doc["assigned_files"], doc["new_destinations"])
		}
		assertJSONModeStreams(t, stdout, stderr, "plan: nothing to plan")
	})

	// partial/absent (design): no cell - plan commits in one
	// transaction or not at all (DESIGN_SPEC 4.5).
	//
	// PROBED TRUE (the T6.2 review's "is the documentation claim
	// real?" item): a mid-run WRITE failure - the assignment
	// transaction itself, driven into a read-only catalog - under
	// --json emits NO document at all, not even a half-written one:
	// runPlanReport returns the write error BEFORE writePlanReport,
	// so stdout stays empty and a parser never sees a partial report.
	// The reachable failure class is the fatal one (exit 1), never a
	// per-item class.
	t.Run("partial (design-absent: mid-run write failure, no document)", func(t *testing.T) {
		if asRoot() {
			t.Skip("running as root: chmod 0444 does not bind writes")
		}
		root := makeFixture(t)
		cat := createCatalog(t)
		scanPlanFixture(t, root, cat, "S-JPFAIL") // scanned, NEVER planned
		if err := os.Chmod(cat, 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(cat, 0o644) })
		stdout, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", "S-JPFAIL", "--size", planFixtureSize, "--json")
		if code == report.ExitOK {
			t.Fatalf("plan over a read-only catalog exited 0 (the write failure did not bind)\nstderr: %s", stderr)
		}
		if stdout != "" {
			t.Errorf("a mid-run plan failure must emit NO document under --json (no half-written report); stdout = %q", stdout)
		}
		if !strings.Contains(stderr, "isopress: error:") {
			t.Errorf("stderr lacks the error diagnostic:\n%s", stderr)
		}
	})
}

// --- report ----------------------------------------------------------------------

// TestGoldenJSONSchemaReport covers report's SUCCESS (the read-only
// projection over an unplanned set) and EMPTY (the fully-planned set:
// nothing left to plan). PARTIAL is design-absent for the same
// transactional reason as plan (report is the read-only mode of the
// identical pipeline).
func TestGoldenJSONSchemaReport(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-JREP")

	t.Run("success (projection)", func(t *testing.T) {
		stdout, stderr, code := runReportChild(t,
			"--catalog", cat, "--set-id", "S-JREP", "--size", planFixtureSize, "--json")
		if code != report.ExitOK {
			t.Fatalf("report --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, planReportJSONKeys)
		assertJSONTypes(t, doc, planReportJSONTypes)
		if doc["mode"] != "report" || doc["set_id"] != "S-JREP" {
			t.Errorf("identity = %v/%v, want report/S-JREP", doc["mode"], doc["set_id"])
		}
		if doc["nothing_to_plan"] != false {
			t.Errorf("nothing_to_plan = %v, want false (nothing assigned yet)", doc["nothing_to_plan"])
		}
		assertJSONModeStreams(t, stdout, stderr, "report: media")
	})

	t.Run("empty (fully planned)", func(t *testing.T) {
		if _, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", "S-JREP", "--size", planFixtureSize); code != report.ExitOK {
			t.Fatalf("fixture plan exit = %d, want 0\nstderr: %s", code, stderr)
		}
		stdout, stderr, code := runReportChild(t,
			"--catalog", cat, "--set-id", "S-JREP", "--size", planFixtureSize, "--json")
		if code != report.ExitOK {
			t.Fatalf("report --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, planReportJSONKeys)
		assertJSONTypes(t, doc, planReportJSONTypes)
		if doc["nothing_to_plan"] != true {
			t.Errorf("nothing_to_plan = %v, want true (the set is fully assigned)", doc["nothing_to_plan"])
		}
		assertJSONModeStreams(t, stdout, stderr, "report: nothing to plan")
	})

	// partial/absent (design): report is strictly read-only (no
	// transaction to fail), so the reachable mid-run failure is a
	// READ failure - PROBED TRUE here with a truncated catalog: the
	// projection dies mid-read and --json emits NO document (the
	// render happens only after a fully successful Balance).
	t.Run("partial (design-absent: mid-run read failure, no document)", func(t *testing.T) {
		root := makeFixture(t)
		cat := createCatalog(t)
		scanPlanFixture(t, root, cat, "S-JRFAIL")
		// Drop the WAL sidecars (a stale WAL over a truncated main
		// file would change the failure CLASS, not the shape) and cut
		// the main file to its first page: every table read past the
		// header fails as corrupt.
		for _, sidecar := range []string{cat + "-wal", cat + "-shm"} {
			_ = os.Remove(sidecar)
		}
		if err := os.Truncate(cat, 4096); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code := runReportChild(t,
			"--catalog", cat, "--set-id", "S-JRFAIL", "--size", planFixtureSize, "--json")
		if code == report.ExitOK {
			t.Fatalf("report over a truncated catalog exited 0\nstderr: %s", stderr)
		}
		if stdout != "" {
			t.Errorf("a mid-run report failure must emit NO document under --json; stdout = %q", stdout)
		}
		if !strings.Contains(stderr, "isopress: error:") {
			t.Errorf("stderr lacks the error diagnostic:\n%s", stderr)
		}
	})
}

// --- clear-assignments ------------------------------------------------------------

// TestGoldenJSONSchemaClearAssignments covers clear's SUCCESS (a
// planned set unlocked) and EMPTY (a scanned-but-unplanned set:
// nothing_to_clear, run_id 0 - the documented no-op floor). PARTIAL is
// design-absent: the clear is one transaction (4.5) or nothing.
func TestGoldenJSONSchemaClearAssignments(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		root := makeFixture(t)
		cat := createCatalog(t)
		scanPlanFixture(t, root, cat, "S-JCLR")
		if _, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", "S-JCLR", "--size", planFixtureSize); code != report.ExitOK {
			t.Fatalf("fixture plan exit = %d, want 0\nstderr: %s", code, stderr)
		}
		stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-JCLR", "--json")
		if code != report.ExitOK {
			t.Fatalf("clear --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, clearReportJSONKeys)
		assertJSONTypes(t, doc, clearJSONTypes)
		if doc["mode"] != "clear" || doc["set_id"] != "S-JCLR" {
			t.Errorf("identity = %v/%v, want clear/S-JCLR", doc["mode"], doc["set_id"])
		}
		if doc["nothing_to_clear"] != false {
			t.Errorf("nothing_to_clear = %v, want false", doc["nothing_to_clear"])
		}
		if doc["files_cleared"].(float64) < 1 || doc["destinations_removed"].(float64) < 1 {
			t.Errorf("cleared = %v files / %v destinations, want a real clear",
				doc["files_cleared"], doc["destinations_removed"])
		}
		assertRunIDShape(t, doc, 1) // a real clear mints a run row
		assertJSONModeStreams(t, stdout, stderr, "clear: cleared")
	})

	t.Run("empty", func(t *testing.T) {
		root := makeFixture(t)
		cat := createCatalog(t)
		scanPlanFixture(t, root, cat, "S-JCLN") // scanned, NEVER planned
		stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-JCLN", "--json")
		if code != report.ExitOK {
			t.Fatalf("empty clear --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, clearReportJSONKeys)
		assertJSONTypes(t, doc, clearJSONTypes)
		if doc["nothing_to_clear"] != true {
			t.Errorf("nothing_to_clear = %v, want true", doc["nothing_to_clear"])
		}
		if doc["run_id"].(float64) != 0 {
			t.Errorf("run_id = %v, want the documented 0 no-op floor (no run row)", doc["run_id"])
		}
		for _, key := range []string{"files_cleared", "chunks_cleared", "dirs_cleared", "destinations_removed"} {
			if doc[key].(float64) != 0 {
				t.Errorf("%s = %v, want 0 on a no-op", key, doc[key])
			}
		}
		// The frozen `unlocked_images` key is present on the completed
		// no-op as an empty array (the T6.2 protocol requires PRESENCE).
		if ids, ok := doc["unlocked_images"].([]any); !ok || len(ids) != 0 {
			t.Errorf("unlocked_images = %#v, want a non-nil empty array on a no-op", doc["unlocked_images"])
		}
		assertJSONModeStreams(t, stdout, stderr, "clear: nothing to clear")
	})

	// The SCOPED run (DESIGN 4.6): the same frozen 10-key document
	// with unlocked_images naming the selected ids (the test plan's
	// "goldenJSONDoc for a scoped run").
	t.Run("scoped", func(t *testing.T) {
		root := makeFixture(t)
		cat := createCatalog(t)
		scanPlanFixture(t, root, cat, "S-JSCOPED")
		if _, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", "S-JSCOPED", "--size", planFixtureSize); code != report.ExitOK {
			t.Fatalf("fixture plan exit = %d, want 0\nstderr: %s", code, stderr)
		}
		stdout, stderr, code := runClearChild(t,
			"--catalog", cat, "--set-id", "S-JSCOPED", "--image", "1", "--json")
		if code != report.ExitOK {
			t.Fatalf("scoped clear --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, clearReportJSONKeys)
		assertJSONTypes(t, doc, clearJSONTypes)
		if doc["nothing_to_clear"] != false {
			t.Errorf("nothing_to_clear = %v, want false", doc["nothing_to_clear"])
		}
		if !reflect.DeepEqual(doc["unlocked_images"], []any{float64(1)}) {
			t.Errorf("unlocked_images = %v, want [1] (the selected ids, sorted)", doc["unlocked_images"])
		}
		if doc["destinations_removed"].(float64) != 1 {
			t.Errorf("destinations_removed = %v, want 1", doc["destinations_removed"])
		}
		assertRunIDShape(t, doc, 1)
		assertJSONModeStreams(t, stdout, stderr, "clear: cleared")
	})

	// partial/absent (design): the clear is one transaction (4.5) -
	// PROBED TRUE with the plan probe: a mid-run WRITE failure (a
	// read-only catalog) under --json emits NO document, exit is the
	// fatal class, never a per-item one.
	t.Run("partial (design-absent: mid-run write failure, no document)", func(t *testing.T) {
		if asRoot() {
			t.Skip("running as root: chmod 0444 does not bind writes")
		}
		root := makeFixture(t)
		cat := createCatalog(t)
		scanPlanFixture(t, root, cat, "S-JCFAIL")
		if _, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", "S-JCFAIL", "--size", planFixtureSize); code != report.ExitOK {
			t.Fatalf("fixture plan exit = %d, want 0\nstderr: %s", code, stderr)
		}
		if err := os.Chmod(cat, 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(cat, 0o644) })
		stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-JCFAIL", "--json")
		if code == report.ExitOK {
			t.Fatalf("clear over a read-only catalog exited 0 (the write failure did not bind)\nstderr: %s", stderr)
		}
		if stdout != "" {
			t.Errorf("a mid-run clear failure must emit NO document under --json; stdout = %q", stdout)
		}
		if !strings.Contains(stderr, "isopress: error:") {
			t.Errorf("stderr lacks the error diagnostic:\n%s", stderr)
		}
	})
}

// --- copy -------------------------------------------------------------------------

// TestGoldenJSONSchemaCopy covers copy's three states: SUCCESS (first
// copy), EMPTY (the steady-state resume: nothing left to copy, exit 0
// with copied 0), PARTIAL (the UTF-8 refusal, exit 10 with the
// enumerated failure AND the warning riding stderr only - the T5.5
// warning-to-stderr leg in --json mode).
func TestGoldenJSONSchemaCopy(t *testing.T) {
	t.Run("success+empty", func(t *testing.T) {
		f := newCopyCrashFixture(t)

		// SUCCESS: the first copy.
		stdout, stderr, code := runCopyChild(t, nil,
			"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest, "--json")
		if code != report.ExitOK {
			t.Fatalf("copy --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, copyJSONKeys)
		assertJSONTypes(t, doc, copyJSONTypes)
		assertArrayElementTypes(t, doc, "images", map[string]string{
			"image": "number", "copied": "number", "already_copied": "number",
			"skipped": "number", "failed": "number", "dirs": "number", "bytes": "number",
			"complete": "bool", "manifest_written": "bool",
		})
		assertImageEntriesShape(t, doc, false)
		if doc["status"] != "ok" || doc["set_id"] != f.setID {
			t.Errorf("identity = %v/%v, want ok/%s", doc["status"], doc["set_id"], f.setID)
		}
		if doc["copied"].(float64) < 1 || doc["failed"].(float64) != 0 {
			t.Errorf("copied/failed = %v/%v, want >=1/0", doc["copied"], doc["failed"])
		}
		assertRunIDShape(t, doc, 1)
		assertAbsPathShape(t, doc, "dest")
		assertJSONModeStreams(t, stdout, stderr, "copy: files", "copy: files 3 copied")

		// EMPTY: the same set re-copied - the steady-state resume has
		// nothing to do: exit 0, copied 0, everything already present.
		stdout, stderr, code = runCopyChild(t, nil,
			"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest, "--json")
		if code != report.ExitOK {
			t.Fatalf("steady-state copy --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc = goldenJSONDoc(t, stdout, copyJSONKeys)
		assertJSONTypes(t, doc, copyJSONTypes)
		assertImageEntriesShape(t, doc, false)
		if doc["status"] != "ok" || doc["copied"].(float64) != 0 || doc["already_copied"].(float64) < 1 {
			t.Errorf("steady report = status %v copied %v already %v, want ok/0/>=1",
				doc["status"], doc["copied"], doc["already_copied"])
		}
		assertRunIDShape(t, doc, 2) // a second, distinct run row
		assertJSONModeStreams(t, stdout, stderr, "copy: files 0 copied (3 already present)")
	})

	t.Run("partial", func(t *testing.T) {
		root := t.TempDir()
		cat := filepath.Join(t.TempDir(), "catalog.sqlite")
		dest := filepath.Join(t.TempDir(), "dest")
		const setID = "S-JUTF8"
		writeFileP(t, filepath.Join(root, "good.txt"), pattern(10, 256), 0o644)
		writeFileP(t, filepath.Join(root, "bad-\xff.txt"), pattern(11, 256), 0o644)
		if _, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root); code != 0 {
			t.Fatalf("fixture scan exit = %d, want 0 (stderr: %s)", code, stderr)
		}
		planSynthetic(t, cat, setID, copySyntheticUsable)

		stdout, stderr, code := runCopyChild(t, nil,
			"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
		if code != report.ExitItemFailures {
			t.Fatalf("partial copy --json exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		wantKeys := append(append([]string(nil), copyJSONKeys...), "failures")
		doc := goldenJSONDoc(t, stdout, wantKeys)
		assertJSONTypes(t, doc, copyJSONTypes)
		assertJSONTypes(t, doc, map[string]string{"failures": "array"})
		assertImageEntriesShape(t, doc, false)
		if doc["status"] != "partial" || doc["skipped"].(float64) != 1 {
			t.Errorf("report = status %v skipped %v, want partial/1", doc["status"], doc["skipped"])
		}
		failures, _ := doc["failures"].([]any)
		if len(failures) != 1 {
			t.Fatalf("failures = %v, want one enumerated skip line", doc["failures"])
		}
		line, _ := failures[0].(string)
		if !strings.Contains(line, "not valid UTF-8") {
			t.Errorf("failure line %q lacks the fidelity reason", line)
		}
		assertRunIDShape(t, doc, 1)
		assertAbsPathShape(t, doc, "dest")
		// THE T5.5 WARNING LEG under --json: the rendered warning is
		// on stderr, stdout carries only the structured failures list.
		assertJSONModeStreams(t, stdout, stderr,
			"copy: files", "warning: image 1:", "isopress: error: completed with 1 item failure")
	})
}

// --- build ------------------------------------------------------------------------

// TestGoldenJSONSchemaBuild covers build's SUCCESS (fake xorriso,
// --verify-after-build=false - the documented V1 reading) and PARTIAL
// (step-7 self-verify failure, exit 10 with the per-image failure).
// EMPTY is design-absent: build has no no-op mode - an unplanned set
// is the usage-class "no planned images" refusal, pinned here as "no
// document on stdout under --json" (and its exit code is identical
// with and without --json - see TestJSONModeExitCodesMatchHumanMode).
func TestGoldenJSONSchemaBuild(t *testing.T) {
	root := makeBuildSingleImageTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, "S-JBUILD")
	planSynthetic(t, cat, "S-JBUILD", buildFixtureProfile)
	fakePath, _ := installFakeXorriso(t)

	t.Run("success", func(t *testing.T) {
		stdout, stderr, code := runBuildChildEnv(t, []string{"PATH=" + fakePath},
			"build", "--catalog", cat, "--set-id", "S-JBUILD", "--dest", filepath.Join(t.TempDir(), "dest"),
			"--verify-after-build=false", "--json")
		if code != report.ExitOK {
			t.Fatalf("build --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, buildJSONKeys)
		assertJSONTypes(t, doc, buildJSONTypes)
		assertArrayElementTypes(t, doc, "images", map[string]string{
			"image": "number", "status": "string", "iso_path": "string", "bytes": "number",
			"content_entries": "number", "verified_entries": "number",
			"verify_skipped": "bool", "warnings": "number",
		})
		assertImageEntriesShape(t, doc, false)
		if doc["status"] != "ok" || doc["built"].(float64) != 1 || doc["failed"].(float64) != 0 {
			t.Errorf("report = status %v built %v failed %v, want ok/1/0",
				doc["status"], doc["built"], doc["failed"])
		}
		assertRunIDShape(t, doc, 1)
		assertAbsPathShape(t, doc, "dest")
		assertJSONModeStreams(t, stdout, stderr, "image(s) built")
	})

	t.Run("partial", func(t *testing.T) {
		stdout, stderr, code := runBuildChildEnv(t, []string{"PATH=" + fakePath},
			"build", "--catalog", cat, "--set-id", "S-JBUILD", "--dest", filepath.Join(t.TempDir(), "dest2"),
			"--json")
		if code != report.ExitItemFailures {
			t.Fatalf("partial build --json exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		wantKeys := append(append([]string(nil), buildJSONKeys...), "failures")
		doc := goldenJSONDoc(t, stdout, wantKeys)
		assertJSONTypes(t, doc, buildJSONTypes)
		assertJSONTypes(t, doc, map[string]string{"failures": "array"})
		assertArrayElementTypes(t, doc, "images", map[string]string{
			"image": "number", "status": "string", "bytes": "number",
			"content_entries": "number", "verified_entries": "number",
			"verify_skipped": "bool", "warnings": "number", "failure": "string",
		})
		assertImageEntriesShape(t, doc, false)
		if doc["status"] != "partial" || doc["failed"].(float64) != 1 {
			t.Errorf("report = status %v failed %v, want partial/1", doc["status"], doc["failed"])
		}
		images, _ := doc["images"].([]any)
		if len(images) != 1 {
			t.Fatalf("images = %v, want one failed image", doc["images"])
		}
		img := images[0].(map[string]any)
		if img["status"] != "failed" || !strings.Contains(img["failure"].(string), "extraction") {
			t.Errorf("image report = %v, want failed carrying the extraction failure", img)
		}
		assertRunIDShape(t, doc, 1)
		assertAbsPathShape(t, doc, "dest")
		assertJSONModeStreams(t, stdout, stderr, "error: build: image 1", "image(s) built",
			"isopress: error: completed with 1 item failure")
	})

	t.Run("empty (design-absent: usage gate, no document)", func(t *testing.T) {
		gateCat := createCatalog(t)
		scanBuildFixture(t, makeBuildSingleImageTree(t), gateCat, "S-JBGATE")
		stdout, stderr, code := runBuildChildEnv(t, []string{"PATH=" + fakePath},
			"build", "--catalog", gateCat, "--set-id", "S-JBGATE", "--dest", t.TempDir(), "--json")
		if code != report.ExitUsage {
			t.Fatalf("unplanned build --json exit = %d, want 2 (usage)\nstderr: %s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("--json must not fabricate a report on an abort-before-work class; stdout = %q", stdout)
		}
		if !strings.Contains(stderr, "no planned images") {
			t.Errorf("stderr lacks the gate refusal:\n%s", stderr)
		}
	})
}

// --- verify-media -----------------------------------------------------------------

// TestGoldenJSONSchemaVerifyMedia covers verify-media's SUCCESS (clean
// copy tree stamped) and PARTIAL (planted corruption, exit 10 with the
// per-image problems). EMPTY is design-absent - the unplanned-set
// usage gate is pinned as "no document" below (and its exit code is
// json-stable per TestJSONModeExitCodesMatchHumanMode).
func TestGoldenJSONSchemaVerifyMedia(t *testing.T) {
	cat, dest := verifyCopyFixture(t, "S-JVERIFY")

	t.Run("success", func(t *testing.T) {
		stdout, stderr, code := runVerifyMediaChild(t, nil,
			"verify-media", "--catalog", cat, "--set-id", "S-JVERIFY", "--json")
		if code != report.ExitOK {
			t.Fatalf("verify-media --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, verifyMediaJSONKeys)
		assertJSONTypes(t, doc, verifyMediaJSONTypes)
		assertArrayElementTypes(t, doc, "images", map[string]string{
			"image": "number", "path": "string", "source": "string", "status": "string",
			"matched": "number", "mismatched": "number", "missing": "number",
			"unexpected": "number", "unreadable": "number", "stamped": "bool",
		})
		assertImageEntriesShape(t, doc, true)
		if doc["status"] != "ok" || doc["verified"].(float64) != 1 {
			t.Errorf("report = status %v verified %v, want ok/1", doc["status"], doc["verified"])
		}
		images, _ := doc["images"].([]any)
		if len(images) != 1 {
			t.Fatalf("images = %v, want one entry", doc["images"])
		}
		if images[0].(map[string]any)["stamped"] != true {
			t.Errorf("image report lacks the 7.2 stamp: %v", images[0])
		}
		assertJSONModeStreams(t, stdout, stderr, "verify(hash): files", "verify-media: 1 image(s) verified")
	})

	t.Run("partial", func(t *testing.T) {
		imgDir := destImageDir(dest, "S-JVERIFY", 1)
		p := filepath.Join(imgDir, "files", "small1.txt")
		orig, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		orig[0] ^= 0xff
		if err := os.WriteFile(p, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code := runVerifyMediaChild(t, nil,
			"verify-media", "--catalog", cat, "--set-id", "S-JVERIFY", "--json")
		if code != report.ExitItemFailures {
			t.Fatalf("partial verify-media --json exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, verifyMediaJSONKeys)
		assertJSONTypes(t, doc, verifyMediaJSONTypes)
		assertArrayElementTypes(t, doc, "images", map[string]string{
			"image": "number", "path": "string", "source": "string", "status": "string",
			"matched": "number", "mismatched": "number", "missing": "number",
			"unexpected": "number", "unreadable": "number", "stamped": "bool",
			"problems": "array",
		})
		assertImageEntriesShape(t, doc, true)
		if doc["status"] != "partial" || doc["mismatched"].(float64) < 1 || doc["verified"].(float64) != 0 {
			t.Errorf("report = status %v mismatched %v verified %v, want partial/>=1/0",
				doc["status"], doc["mismatched"], doc["verified"])
		}
		images, _ := doc["images"].([]any)
		img := images[0].(map[string]any)
		if img["status"] != "mismatch" || img["stamped"] != false {
			t.Errorf("image report = %v, want an unstamped mismatch", img)
		}
		problems, _ := img["problems"].([]any)
		if len(problems) < 1 {
			t.Fatalf("problems = %v, want the enumerated per-file findings", img["problems"])
		}
		if line, _ := problems[0].(string); !strings.Contains(line, "small1.txt") {
			t.Errorf("problem line %q does not name the corrupt file", line)
		}
		assertJSONModeStreams(t, stdout, stderr, "verify(hash): files",
			"verify-media: 0 image(s) verified", "isopress: error: completed with 1 item failure")
	})

	t.Run("empty (design-absent: usage gate, no document)", func(t *testing.T) {
		gateCat := createCatalog(t)
		scanBuildFixture(t, makeBuildSingleImageTree(t), gateCat, "S-JVGATE")
		stdout, stderr, code := runVerifyMediaChild(t, nil,
			"verify-media", "--catalog", gateCat, "--set-id", "S-JVGATE", "--json")
		if code != report.ExitUsage {
			t.Fatalf("unplanned verify-media --json exit = %d, want 2 (usage)\nstderr: %s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("--json must not fabricate a report on the gate; stdout = %q", stdout)
		}
		if !strings.Contains(stderr, "no planned images") {
			t.Errorf("stderr lacks the gate refusal:\n%s", stderr)
		}
	})
}

// --- validate ---------------------------------------------------------------------

// TestGoldenJSONSchemaValidate covers validate's SUCCESS (clean set),
// EMPTY (a scanned empty root: zero records, zero findings - the
// findings array is present and empty in both, the frozen-shape
// guarantee) and PARTIAL (the mutation matrix, exit 10 with the
// enumerated findings).
func TestGoldenJSONSchemaValidate(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		const setID = "S-JVCLEAN"
		_, cat := validateFixture(t, setID)
		stdout, stderr, code := runValidateChild(t, nil,
			"validate", "--catalog", cat, "--set-id", setID, "--json")
		if code != report.ExitOK {
			t.Fatalf("clean validate --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, validateJSONKeys)
		assertJSONTypes(t, doc, validateJSONTypes)
		if doc["status"] != "ok" || doc["anomalies"].(float64) != 0 {
			t.Errorf("report = status %v anomalies %v, want ok/0", doc["status"], doc["anomalies"])
		}
		files, ok := doc["files"].([]any)
		if !ok || len(files) != 0 {
			t.Errorf("files = %#v, want a non-nil empty findings array (frozen shape)", doc["files"])
		}
		assertAbsPathShape(t, doc, "root")
		assertJSONModeStreams(t, stdout, stderr, "validate: files", "validate: set "+setID)
	})

	t.Run("empty", func(t *testing.T) {
		const setID = "S-JVEMPTY"
		root := t.TempDir() // scanned empty root: zero records
		cat := createCatalog(t)
		scanBuildFixture(t, root, cat, setID)
		stdout, stderr, code := runValidateChild(t, nil,
			"validate", "--catalog", cat, "--set-id", setID, "--json")
		if code != report.ExitOK {
			t.Fatalf("empty validate --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, validateJSONKeys)
		assertJSONTypes(t, doc, validateJSONTypes)
		if doc["status"] != "ok" || doc["records"].(float64) != 0 || doc["anomalies"].(float64) != 0 {
			t.Errorf("empty report = status %v records %v anomalies %v, want ok/0/0",
				doc["status"], doc["records"], doc["anomalies"])
		}
		if files, ok := doc["files"].([]any); !ok || len(files) != 0 {
			t.Errorf("files = %#v, want an empty findings array", doc["files"])
		}
		assertAbsPathShape(t, doc, "root")
		assertJSONModeStreams(t, stdout, stderr, "validate: set "+setID, "compared 0 records")
	})

	t.Run("partial", func(t *testing.T) {
		const setID = "S-JVPART"
		root, cat := validateFixture(t, setID)
		want := mutateValidateSource(t, root).withAnomalies()
		if want.Anomalies == 0 {
			t.Fatal("fixture produced no anomalies")
		}
		stdout, stderr, code := runValidateChild(t, nil,
			"validate", "--catalog", cat, "--set-id", setID, "--json")
		if code != report.ExitItemFailures {
			t.Fatalf("partial validate --json exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, validateJSONKeys)
		assertJSONTypes(t, doc, validateJSONTypes)
		assertArrayElementTypes(t, doc, "files", map[string]string{
			"category": "string", "path": "string",
		})
		if doc["status"] != "partial" || doc["anomalies"].(float64) != float64(want.Anomalies) {
			t.Errorf("report = status %v anomalies %v, want partial/%d",
				doc["status"], doc["anomalies"], want.Anomalies)
		}
		files, _ := doc["files"].([]any)
		if len(files) != want.Anomalies+want.New {
			t.Errorf("findings = %d, want anomalies(%d)+new(%d)", len(files), want.Anomalies, want.New)
		}
		// The per-item listing carries the failures: at least one
		// finding names a path and a category class.
		first := files[0].(map[string]any)
		if first["category"] == "ok" || first["path"] == "" {
			t.Errorf("finding = %v, want a non-ok class with a path", first)
		}
		assertAbsPathShape(t, doc, "root")
		assertJSONModeStreams(t, stdout, stderr, "validate: set "+setID,
			"isopress: error: completed with")
	})
}

// --- restore ----------------------------------------------------------------------

// TestGoldenJSONSchemaRestore covers restore's SUCCESS (whole-set
// restore through the recording seam) and PARTIAL (one injected member
// failure, exit 10 with the per-file finding). EMPTY is design-absent:
// an empty selection is refused by the selector/plan gates (the
// abort-before-work classes) - pinned as "no document under --json"
// below, with the same exit code as the human mode.
func TestGoldenJSONSchemaRestore(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-JRESTORE")

	t.Run("success", func(t *testing.T) {
		useRestoreFake(t)
		stdout, stderr, code := runInProcess(t, "restore", "--json",
			"--catalog", cat, "--set-id", "S-JRESTORE", "--target", restoreTarget(t),
			"--image", "1="+destImageDir(dest, "S-JRESTORE", 1),
			"--image", "2="+destImageDir(dest, "S-JRESTORE", 2))
		if code != report.ExitOK {
			t.Fatalf("restore --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, restoreJSONKeys)
		assertJSONTypes(t, doc, restoreJSONTypes)
		assertArrayElementTypes(t, doc, "images", map[string]string{
			"image": "number", "path": "string", "source": "string", "status": "string",
			"restored": "number", "skipped": "number", "failed": "number", "bytes": "number",
			"files": "array",
		})
		assertImageEntriesShape(t, doc, true)
		if doc["status"] != "ok" || doc["selection"] != "whole-set" {
			t.Errorf("identity = %v/%v, want ok/whole-set", doc["status"], doc["selection"])
		}
		if doc["restored"].(float64) < 1 {
			t.Errorf("restored = %v, want >= 1", doc["restored"])
		}
		unclaimed, ok := doc["unclaimed"].([]any)
		if !ok || len(unclaimed) != 0 {
			t.Errorf("unclaimed = %#v, want a non-nil empty array (frozen shape)", doc["unclaimed"])
		}
		assertAbsPathShape(t, doc, "target")
		assertJSONModeStreams(t, stdout, stderr, "restore: set S-JRESTORE", "status ok")
	})

	t.Run("partial", func(t *testing.T) {
		useRestoreFake(t, "small1.txt") // one member fails
		stdout, stderr, code := runInProcess(t, "restore", "--json",
			"--catalog", cat, "--set-id", "S-JRESTORE", "--target", restoreTarget(t),
			"--image", "1="+destImageDir(dest, "S-JRESTORE", 1),
			"--image", "2="+destImageDir(dest, "S-JRESTORE", 2))
		if code != report.ExitItemFailures {
			t.Fatalf("partial restore --json exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := goldenJSONDoc(t, stdout, restoreJSONKeys)
		assertJSONTypes(t, doc, restoreJSONTypes)
		assertArrayElementTypes(t, doc, "images", map[string]string{
			"image": "number", "path": "string", "source": "string", "status": "string",
			"restored": "number", "skipped": "number", "failed": "number", "bytes": "number",
			"files": "array",
		})
		assertImageEntriesShape(t, doc, true)
		if doc["status"] != "partial" || doc["failed"].(float64) != 1 {
			t.Errorf("report = status %v failed %v, want partial/1", doc["status"], doc["failed"])
		}
		// The per-item listing carries the failure: the failing image
		// enumerates the member.
		images, _ := doc["images"].([]any)
		var finding map[string]any
		for _, e := range images {
			img := e.(map[string]any)
			if img["status"] != "restored" {
				findings, _ := img["files"].([]any)
				if len(findings) > 0 {
					finding = findings[0].(map[string]any)
				}
			}
		}
		if finding == nil {
			t.Fatalf("no image enumerates the failed member: %v", doc["images"])
		}
		if finding["path"] != "small1.txt" || finding["status"] != "failed" {
			t.Errorf("finding = %v, want small1.txt/failed", finding)
		}
		assertAbsPathShape(t, doc, "target")
		assertJSONModeStreams(t, stdout, stderr, "restore: set S-JRESTORE", "status partial",
			"isopress: error: completed with 1 item failure")
	})

	t.Run("empty (design-absent: usage gate, no document)", func(t *testing.T) {
		gateCat := createCatalog(t)
		scanBuildFixture(t, makeBuildSingleImageTree(t), gateCat, "S-JRGATE")
		stdout, stderr, code := runInProcess(t, "restore", "--json",
			"--catalog", gateCat, "--set-id", "S-JRGATE", "--target", restoreTarget(t))
		if code != report.ExitUsage {
			t.Fatalf("unplanned restore --json exit = %d, want 2 (usage)\nstderr: %s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("--json must not fabricate a report on the gate; stdout = %q", stdout)
		}
		if !strings.Contains(stderr, "no planned images") {
			t.Errorf("stderr lacks the gate refusal:\n%s", stderr)
		}
	})
}

// --- byte hygiene of the --json stdout stream ------------------------------------

// TestJSONStdoutHasNoStrayBytes pins the byte-level shape of a --json
// stdout stream for three commands with three different report
// writers (scan/copy/restore): no UTF-8 BOM, the stream starts with
// the opening brace, and it ends with the closing brace followed by
// EXACTLY ONE newline. encoding/json.Decode tolerates leading
// whitespace and trailing whitespace - that tolerance is FINE for the
// single-document decode pins above, but a BOM or a second trailing
// newline/garbage is a hazard for naive byte-level consumers and a
// symptom of a stray write, so it is pinned out here.
func TestJSONStdoutHasNoStrayBytes(t *testing.T) {
	t.Run("scan", func(t *testing.T) {
		root := makeFixture(t)
		cat := createCatalog(t)
		args := []string{"--catalog", cat, "--set-id", "S-JBYTE", "--json"}
		for _, p := range fixtureExcludes() {
			args = append(args, "--exclude", p)
		}
		stdout, stderr, code := runScanChild(t, append(args, root)...)
		if code != report.ExitOK {
			t.Fatalf("scan --json exit = %d, want 0\nstderr: %s", code, stderr)
		}
		assertJSONStdoutBytesClean(t, stdout)
	})
	t.Run("copy", func(t *testing.T) {
		f := newCopyCrashFixture(t)
		stdout, stderr, code := runCopyChild(t, nil,
			"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest, "--json")
		if code != report.ExitOK {
			t.Fatalf("copy --json exit = %d, want 0\nstderr: %s", code, stderr)
		}
		assertJSONStdoutBytesClean(t, stdout)
	})
	t.Run("restore", func(t *testing.T) {
		cat, dest, _ := restoreTwoImageFixture(t, "S-JBYTER")
		useRestoreFake(t)
		stdout, stderr, code := runInProcess(t, "restore", "--json",
			"--catalog", cat, "--set-id", "S-JBYTER", "--target", restoreTarget(t),
			"--image", "1="+destImageDir(dest, "S-JBYTER", 1),
			"--image", "2="+destImageDir(dest, "S-JBYTER", 2))
		if code != report.ExitOK {
			t.Fatalf("restore --json exit = %d, want 0\nstderr: %s", code, stderr)
		}
		assertJSONStdoutBytesClean(t, stdout)
	})
}

// --- stream separation, end to end in --json mode (10.3, per command) -------------

// TestJSONModeStreamSeparationEveryCommand is the brief's item 2: ONE
// --json run PER COMMAND through the capturing renderer (every runner
// captures both process streams; stderr's non-TTY state is what 9.2
// keys the plain renderer on, so the human renderer renders into the
// captured stderr), asserting:
//
//   - stdout carries EXACTLY ONE parseable JSON document and NOTHING
//     else - the piping semantics: the raw stdout bytes parse with
//     encoding/json in ONE Decode, and a second Decode returns io.EOF
//     (decodeSingleJSONDoc does both; a progress line before the
//     document, a trailing summary, or any junk after it fails);
//   - stderr carries the human progress/diagnostics for the same run;
//   - no progress line and no warning rides stdout (the T5.5
//     warning-to-stderr behavior holds in --json mode - the warning
//     row proves it on a run that actually WARNS).
//
// Deliberately END-TO-END over the real command tree: T6.1's
// stream_separation_test.go AST grep covers direct writes from
// service/catalog code; this covers the composed command runs. The
// richer per-state schemas live in TestGoldenJSONSchema* above, which
// call the same assertJSONModeStreams helper.
func TestJSONModeStreamSeparationEveryCommand(t *testing.T) {
	rows := []struct {
		command string
		want    int
		// run executes ONE representative --json invocation of the
		// command (with its cheapest fixture) and returns both streams
		// plus the human markers expected on stderr for that run.
		run     func(t *testing.T) (stdout, stderr string, code int)
		markers []string
	}{
		{
			command: "scan",
			want:    report.ExitOK,
			run: func(t *testing.T) (string, string, int) {
				root := makeFixture(t)
				cat := createCatalog(t)
				args := []string{"--catalog", cat, "--set-id", "S-JSSCAN", "--json"}
				for _, p := range fixtureExcludes() {
					args = append(args, "--exclude", p)
				}
				return runScanChild(t, append(args, root)...)
			},
			markers: []string{"scan(hash): files"},
		},
		{
			command: "plan",
			want:    report.ExitOK,
			run: func(t *testing.T) (string, string, int) {
				root := makeFixture(t)
				cat := createCatalog(t)
				scanPlanFixture(t, root, cat, "S-JSPLAN")
				return runPlanChild(t, "--catalog", cat, "--set-id", "S-JSPLAN",
					"--size", planFixtureSize, "--json")
			},
			markers: []string{"plan: media"},
		},
		{
			command: "report",
			want:    report.ExitOK,
			run: func(t *testing.T) (string, string, int) {
				root := makeFixture(t)
				cat := createCatalog(t)
				scanPlanFixture(t, root, cat, "S-JSREP")
				return runReportChild(t, "--catalog", cat, "--set-id", "S-JSREP",
					"--size", planFixtureSize, "--json")
			},
			markers: []string{"report: media"},
		},
		{
			command: "clear-assignments",
			want:    report.ExitOK,
			run: func(t *testing.T) (string, string, int) {
				root := makeFixture(t)
				cat := createCatalog(t)
				scanPlanFixture(t, root, cat, "S-JSCLEAR") // scanned, unplanned
				return runClearChild(t, "--catalog", cat, "--set-id", "S-JSCLEAR", "--json")
			},
			markers: []string{"clear: nothing to clear"},
		},
		{
			command: "copy",
			want:    report.ExitOK,
			run: func(t *testing.T) (string, string, int) {
				f := newCopyCrashFixture(t)
				return runCopyChild(t, nil, "copy", "--catalog", f.cat,
					"--set-id", f.setID, "--dest", f.dest, "--json")
			},
			markers: []string{"copy: files"},
		},
		{
			command: "build",
			want:    report.ExitOK,
			run: func(t *testing.T) (string, string, int) {
				root := makeBuildSingleImageTree(t)
				cat := createCatalog(t)
				scanBuildFixture(t, root, cat, "S-JSBUILD")
				planSynthetic(t, cat, "S-JSBUILD", buildFixtureProfile)
				fakePath, _ := installFakeXorriso(t)
				return runBuildChildEnv(t, []string{"PATH=" + fakePath}, "build",
					"--catalog", cat, "--set-id", "S-JSBUILD", "--dest", filepath.Join(t.TempDir(), "dest"),
					"--verify-after-build=false", "--json")
			},
			markers: []string{"build(stage): files", "image(s) built"},
		},
		{
			command: "verify-media",
			want:    report.ExitOK,
			run: func(t *testing.T) (string, string, int) {
				cat, _ := verifyCopyFixture(t, "S-JSVERIFY")
				return runVerifyMediaChild(t, nil, "verify-media",
					"--catalog", cat, "--set-id", "S-JSVERIFY", "--json")
			},
			markers: []string{"verify(hash): files", "verify-media: 1 image(s) verified"},
		},
		{
			command: "validate",
			want:    report.ExitOK,
			run: func(t *testing.T) (string, string, int) {
				_, cat := validateFixture(t, "S-JSVALIDATE")
				return runValidateChild(t, nil, "validate",
					"--catalog", cat, "--set-id", "S-JSVALIDATE", "--json")
			},
			markers: []string{"validate: set S-JSVALIDATE"},
		},
		{
			command: "restore",
			want:    report.ExitOK,
			run: func(t *testing.T) (string, string, int) {
				cat, dest, _ := restoreTwoImageFixture(t, "S-JSRESTORE")
				useRestoreFake(t)
				return runInProcess(t, "restore", "--json",
					"--catalog", cat, "--set-id", "S-JSRESTORE", "--target", restoreTarget(t),
					"--image", "1="+destImageDir(dest, "S-JSRESTORE", 1),
					"--image", "2="+destImageDir(dest, "S-JSRESTORE", 2))
			},
			markers: []string{"restore: set S-JSRESTORE"},
		},
	}

	for _, row := range rows {
		t.Run(row.command, func(t *testing.T) {
			stdout, stderr, code := row.run(t)
			if code != row.want {
				t.Fatalf("--json exit = %d, want %d\nstdout: %s\nstderr: %s", code, row.want, stdout, stderr)
			}
			// EXACTLY ONE document, nothing before or after it (the
			// single-Decode + io.EOF pipe contract).
			decodeSingleJSONDoc(t, stdout)
			// Human channel on stderr, never on stdout.
			assertJSONModeStreams(t, stdout, stderr, row.markers...)
		})
	}

	// THE WARNING ROW: a run that actually warns in --json mode - the
	// warning must ride stderr only (T5.5-era behavior), stdout stays
	// the single document. (Skipped as root: the warning needs an
	// unreadable source.)
	if !asRoot() {
		t.Run("scan warning stays on stderr", func(t *testing.T) {
			root := makeFixture(t)
			locked := filepath.Join(root, "locked.dat")
			if err := os.WriteFile(locked, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(locked, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
			cat := createCatalog(t)
			args := []string{"--catalog", cat, "--set-id", "S-JSWARN", "--json"}
			for _, p := range fixtureExcludes() {
				args = append(args, "--exclude", p)
			}
			stdout, stderr, code := runScanChild(t, append(args, root)...)
			if code != report.ExitItemFailures {
				t.Fatalf("--json exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			decodeSingleJSONDoc(t, stdout)
			assertJSONModeStreams(t, stdout, stderr,
				"warning: unreadable file locked.dat", "isopress: error: completed with 1 item failure")
		})
	}
}

// --- exit-code parity (--json must never change a run's exit code) ----------------

// TestJSONModeExitCodesMatchHumanMode is the T6.2 exit-code contract:
// the SAME scenario with and without --json exits IDENTICALLY
// (DESIGN_SPEC 10.2/FR-046: --json selects a presentation, never a
// verdict). One spot-check per command; where the scenario is
// state-changing (a first plan/clear/copy consumes the state), each
// leg gets a fresh but EQUIVALENT fixture, so both legs really run the
// same class of work. The non-zero classes (10) are preferred where
// reachable: they are the codes a --json regression would most
// plausibly disturb.
//
// STRENGTHENED (the T6.2 review): matching exit codes alone would be
// hollow if the two legs' fixtures diverged - e.g. the JSON leg
// copying one file while the human leg copied two could still produce
// the same code. Every leg therefore also asserts WORK-EVIDENCE
// PARITY: the human presentation's counts (the Done summary on
// stderr, the totals line on stdout for copy) must EQUAL the JSON
// document's counts for the same run, so "equivalent fixtures" is a
// machine-checked claim per command.
func TestJSONModeExitCodesMatchHumanMode(t *testing.T) {
	// scan: the partial class, two equivalent locked fixtures.
	//
	// WORK-EVIDENCE PARITY (the T6.2 review's "do both legs really do
	// the same work?" item): beyond the exit code, the JSON leg's
	// document counts must EQUAL the human leg's Done-summary counts -
	// a fixture that diverged between the legs (say, the lock not
	// binding on one leg) would now fail here even if the exit code
	// happened to match.
	t.Run("scan", func(t *testing.T) {
		if asRoot() {
			t.Skip("running as root: chmod 000 does not make files unreadable")
		}
		run := func(json bool) (stdout, stderr string, code int) {
			root := makeFixture(t)
			locked := filepath.Join(root, "locked.dat")
			if err := os.WriteFile(locked, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(locked, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
			cat := createCatalog(t)
			args := []string{"--catalog", cat, "--set-id", "S-JPARITY"}
			if json {
				args = append(args, "--json")
			}
			for _, p := range fixtureExcludes() {
				args = append(args, "--exclude", p)
			}
			return runScanChild(t, append(args, root)...)
		}
		_, humanStderr, humanCode := run(false)
		jsonStdout, _, jsonCode := run(true)
		if humanCode != report.ExitItemFailures || jsonCode != humanCode {
			t.Fatalf("scan exit codes: human=%d json=%d, want both %d",
				humanCode, jsonCode, report.ExitItemFailures)
		}
		doc := decodeSingleJSONDoc(t, jsonStdout)
		// Both legs scanned the SAME tree: 4 fixture files + the
		// locked one, with exactly one unreadable.
		jsonScanned := jsonNum(t, doc, "scanned")
		humanScanned := extractNums(t, "scan summary",
			`scan\(hash\): files (\d+), `, humanStderr, true)[0]
		if jsonScanned != humanScanned || jsonScanned != 5 {
			t.Errorf("scan work evidence: json scanned=%d, human summary scanned=%d, want both 5 (4 fixture + locked.dat)",
				jsonScanned, humanScanned)
		}
		if u := jsonNum(t, doc, "unreadable"); u != 1 {
			t.Errorf("json unreadable = %d, want 1 (the locked file)", u)
		}
	})

	// plan: the success class on two equivalent fresh sets. WORK
	// EVIDENCE: the human leg's Done summary (media/files counts) must
	// equal the JSON leg's document counts - both legs planned
	// equivalent fixtures, so any construction divergence fails here.
	t.Run("plan", func(t *testing.T) {
		run := func(json bool) (stdout, stderr string, code int) {
			root := makeFixture(t)
			cat := createCatalog(t)
			scanPlanFixture(t, root, cat, "S-JPARITY")
			args := []string{"--catalog", cat, "--set-id", "S-JPARITY", "--size", planFixtureSize}
			if json {
				args = append(args, "--json")
			}
			return runPlanChild(t, args...)
		}
		_, humanStderr, humanCode := run(false)
		jsonStdout, _, jsonCode := run(true)
		if humanCode != report.ExitOK || jsonCode != humanCode {
			t.Fatalf("plan exit codes: human=%d json=%d, want both 0", humanCode, jsonCode)
		}
		doc := decodeSingleJSONDoc(t, jsonStdout)
		// One pass, two captures: media and files from the summary line.
		humanCounts := extractNums(t, "plan summary",
			`plan: media (\d+) \(\d+ new\); files (\d+), `, humanStderr, true)
		humanMedia, humanFiles := humanCounts[0], humanCounts[1]
		if j := jsonNum(t, doc, "media_count"); j != humanMedia || j < 1 {
			t.Errorf("plan work evidence: json media_count=%d, human summary media=%d, want equal and >= 1", j, humanMedia)
		}
		if j := jsonNum(t, doc, "assigned_files"); j != humanFiles || j < 1 {
			t.Errorf("plan work evidence: json assigned_files=%d, human summary files=%d, want equal and >= 1", j, humanFiles)
		}
	})

	// report: READ-ONLY - both legs run the identical state in any
	// order on one fixture, and the work-evidence counts must agree.
	t.Run("report", func(t *testing.T) {
		root := makeFixture(t)
		cat := createCatalog(t)
		scanPlanFixture(t, root, cat, "S-JPARITY")
		_, humanStderr, humanCode := runReportChild(t, "--catalog", cat, "--set-id", "S-JPARITY", "--size", planFixtureSize)
		jsonStdout, _, jsonCode := runReportChild(t, "--catalog", cat, "--set-id", "S-JPARITY", "--size", planFixtureSize, "--json")
		if humanCode != report.ExitOK || jsonCode != humanCode {
			t.Fatalf("report exit codes: human=%d json=%d, want both 0", humanCode, jsonCode)
		}
		doc := decodeSingleJSONDoc(t, jsonStdout)
		humanCounts := extractNums(t, "report summary",
			`report: media (\d+) \(\d+ new\); files (\d+), `, humanStderr, true)
		if j := jsonNum(t, doc, "media_count"); j != humanCounts[0] {
			t.Errorf("report work evidence: json media_count=%d, human summary media=%d", j, humanCounts[0])
		}
		if j := jsonNum(t, doc, "assigned_files"); j != humanCounts[1] {
			t.Errorf("report work evidence: json assigned_files=%d, human summary files=%d", j, humanCounts[1])
		}
	})

	// clear: the success class on two equivalent planned sets. WORK
	// EVIDENCE: cleared files/chunks/dirs/destinations in the JSON
	// document must equal the human leg's Done summary counts.
	t.Run("clear-assignments", func(t *testing.T) {
		run := func(json bool) (stdout, stderr string, code int) {
			root := makeFixture(t)
			cat := createCatalog(t)
			scanPlanFixture(t, root, cat, "S-JPARITY")
			if _, _, code := runPlanChild(t,
				"--catalog", cat, "--set-id", "S-JPARITY", "--size", planFixtureSize); code != report.ExitOK {
				t.Fatalf("fixture plan exit = %d, want 0", code)
			}
			args := []string{"--catalog", cat, "--set-id", "S-JPARITY"}
			if json {
				args = append(args, "--json")
			}
			return runClearChild(t, args...)
		}
		_, humanStderr, humanCode := run(false)
		jsonStdout, _, jsonCode := run(true)
		if humanCode != report.ExitOK || jsonCode != humanCode {
			t.Fatalf("clear exit codes: human=%d json=%d, want both 0", humanCode, jsonCode)
		}
		doc := decodeSingleJSONDoc(t, jsonStdout)
		h := extractNums(t, "clear summary",
			`clear: cleared (\d+) files, (\d+) chunks, (\d+) dirs; removed (\d+) destinations`,
			humanStderr, true)
		for jsonKey, humanN := range map[string]int{
			"files_cleared": h[0], "chunks_cleared": h[1],
			"dirs_cleared": h[2], "destinations_removed": h[3],
		} {
			if j := jsonNum(t, doc, jsonKey); j != humanN {
				t.Errorf("clear work evidence: json %s=%d, human summary=%d (legs did diverging work)",
					jsonKey, j, humanN)
			}
		}
		if h[0] == 0 || h[3] == 0 {
			t.Errorf("clear fixture did no work: files=%d destinations=%d, want both >= 1", h[0], h[3])
		}
	})

	// copy: the partial class (source-changed skip -> 10) on two
	// equivalent fixtures. WORK EVIDENCE: the human leg's stdout
	// "totals:" line must carry the SAME copied/already/skipped/failed/
	// dirs numbers as the JSON leg's document - the exact counts both
	// legs actually produced, not just the shared exit code.
	t.Run("copy", func(t *testing.T) {
		run := func(json bool) (stdout, stderr string, code int) {
			root := t.TempDir()
			cat := filepath.Join(t.TempDir(), "catalog.sqlite")
			dest := filepath.Join(t.TempDir(), "dest")
			writeFileP(t, filepath.Join(root, "stable.txt"), pattern(7, 512), 0o644)
			writeFileP(t, filepath.Join(root, "volatile.txt"), pattern(8, 512), 0o644)
			if _, _, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", "S-JPARITY", root); code != 0 {
				t.Fatalf("fixture scan exit = %d, want 0", code)
			}
			planSynthetic(t, cat, "S-JPARITY", copySyntheticUsable)
			// The 5.1 comparison set needs a distinct mtime (the copy
			// suite's own precedent: a 10ms sleep before the mutation).
			time.Sleep(10 * time.Millisecond)
			writeFileP(t, filepath.Join(root, "volatile.txt"), pattern(9, 600), 0o644)
			args := []string{"copy", "--catalog", cat, "--set-id", "S-JPARITY", "--dest", dest}
			if json {
				args = append(args, "--json")
			}
			return runCopyChild(t, nil, args...)
		}
		humanStdout, _, humanCode := run(false)
		jsonStdout, _, jsonCode := run(true)
		if humanCode != report.ExitItemFailures || jsonCode != humanCode {
			t.Fatalf("copy exit codes: human=%d json=%d, want both %d",
				humanCode, jsonCode, report.ExitItemFailures)
		}
		doc := decodeSingleJSONDoc(t, jsonStdout)
		h := extractNums(t, "copy totals",
			`totals: copied (\d+), already-copied (\d+), skipped (\d+), failed (\d+), dirs (\d+)`,
			humanStdout, true)
		for jsonKey, humanN := range map[string]int{
			"copied": h[0], "already_copied": h[1], "skipped": h[2],
			"failed": h[3], "dirs": h[4],
		} {
			if j := jsonNum(t, doc, jsonKey); j != humanN {
				t.Errorf("copy work evidence: json %s=%d, human totals=%d (legs did diverging work)",
					jsonKey, j, humanN)
			}
		}
		// Both legs did the SAME real work: stable.txt copied,
		// volatile.txt skipped as source-changed.
		if h[0] != 1 || h[2] != 1 || h[3] != 0 {
			t.Errorf("copy fixture work: copied=%d skipped=%d failed=%d, want 1/1/0", h[0], h[2], h[3])
		}
	})

	// build: the partial class (step-7 failure -> 10); the failing
	// state REPRODUCES on the second run (the image re-masters and
	// fails again), so one fixture serves both legs. WORK EVIDENCE:
	// the human leg's Done summary (built/failed/warnings) must equal
	// the JSON leg's document counts - the reproduction claim is now
	// asserted numerically, not by construction trust.
	t.Run("build", func(t *testing.T) {
		root := makeBuildSingleImageTree(t)
		cat := createCatalog(t)
		scanBuildFixture(t, root, cat, "S-JPARITY")
		planSynthetic(t, cat, "S-JPARITY", buildFixtureProfile)
		fakePath, _ := installFakeXorriso(t)
		dest := filepath.Join(t.TempDir(), "dest")
		jsonStdout, _, jsonCode := runBuildChildEnv(t, []string{"PATH=" + fakePath},
			"build", "--catalog", cat, "--set-id", "S-JPARITY", "--dest", dest, "--json")
		_, humanStderr, humanCode := runBuildChildEnv(t, []string{"PATH=" + fakePath},
			"build", "--catalog", cat, "--set-id", "S-JPARITY", "--dest", dest)
		if humanCode != report.ExitItemFailures || jsonCode != humanCode {
			t.Fatalf("build exit codes: human=%d json=%d, want both %d",
				humanCode, jsonCode, report.ExitItemFailures)
		}
		doc := decodeSingleJSONDoc(t, jsonStdout)
		h := extractNums(t, "build summary",
			`build: (\d+) image\(s\) built \(.*?\), (\d+) failed, warnings (\d+)`, humanStderr, true)
		for jsonKey, humanN := range map[string]int{
			"built": h[0], "failed": h[1], "warnings": h[2],
		} {
			if j := jsonNum(t, doc, jsonKey); j != humanN {
				t.Errorf("build work evidence: json %s=%d, human summary=%d (the second leg did not reproduce the first)",
					jsonKey, j, humanN)
			}
		}
		if h[0] != 0 || h[1] != 1 {
			t.Errorf("build fixture work: built=%d failed=%d, want 0/1 (the step-7 failure)", h[0], h[1])
		}
	})

	// verify-media: the partial class; verification never mutates the
	// corruption, so one fixture serves both legs. WORK EVIDENCE: the
	// human summary's verified/mismatched counts must equal the JSON
	// document's.
	t.Run("verify-media", func(t *testing.T) {
		cat, dest := verifyCopyFixture(t, "S-JPARITY")
		imgDir := destImageDir(dest, "S-JPARITY", 1)
		p := filepath.Join(imgDir, "files", "small1.txt")
		orig, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		orig[0] ^= 0xff
		if err := os.WriteFile(p, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		jsonStdout, _, jsonCode := runVerifyMediaChild(t, nil,
			"verify-media", "--catalog", cat, "--set-id", "S-JPARITY", "--json")
		_, humanStderr, humanCode := runVerifyMediaChild(t, nil,
			"verify-media", "--catalog", cat, "--set-id", "S-JPARITY")
		if humanCode != report.ExitItemFailures || jsonCode != humanCode {
			t.Fatalf("verify-media exit codes: human=%d json=%d, want both %d",
				humanCode, jsonCode, report.ExitItemFailures)
		}
		doc := decodeSingleJSONDoc(t, jsonStdout)
		h := extractNums(t, "verify-media summary",
			`verify-media: (\d+) image\(s\) verified, (\d+) anomalous \(matched \d+ entries, mismatched (\d+),`,
			humanStderr, true)
		for jsonKey, humanN := range map[string]int{
			"verified": h[0], "mismatched": h[2],
		} {
			if j := jsonNum(t, doc, jsonKey); j != humanN {
				t.Errorf("verify-media work evidence: json %s=%d, human summary=%d",
					jsonKey, j, humanN)
			}
		}
		if h[0] != 0 || h[2] < 1 {
			t.Errorf("verify-media fixture work: verified=%d mismatched=%d, want 0/>=1 (one corrupt image)",
				h[0], h[2])
		}
	})

	// validate: the partial class; validation is read-only, so one
	// mutated fixture serves both legs. WORK EVIDENCE: the human
	// summary's compared/anomalies counts must equal the JSON
	// document's.
	t.Run("validate", func(t *testing.T) {
		const setID = "S-JPARITY"
		root, cat := validateFixture(t, setID)
		mutateValidateSource(t, root)
		jsonStdout, _, jsonCode := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID, "--json")
		_, humanStderr, humanCode := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
		if humanCode != report.ExitItemFailures || jsonCode != humanCode {
			t.Fatalf("validate exit codes: human=%d json=%d, want both %d",
				humanCode, jsonCode, report.ExitItemFailures)
		}
		doc := decodeSingleJSONDoc(t, jsonStdout)
		h := extractNums(t, "validate summary",
			`validate: set \S+: compared (\d+) records, .*\(anomalies (\d+), status`,
			humanStderr, true)
		if j := jsonNum(t, doc, "records"); j != h[0] {
			t.Errorf("validate work evidence: json records=%d, human summary compared=%d", j, h[0])
		}
		if j := jsonNum(t, doc, "anomalies"); j != h[1] || j == 0 {
			t.Errorf("validate work evidence: json anomalies=%d, human summary anomalies=%d (want equal, > 0)",
				j, h[1])
		}
	})

	// restore: the success class; the fake seam is stateless across
	// runs, so one fixture serves both legs. WORK EVIDENCE: the human
	// summary's selected/restored/skipped/failed counts must equal the
	// JSON document's.
	t.Run("restore", func(t *testing.T) {
		cat, dest, _ := restoreTwoImageFixture(t, "S-JPARITY")
		args := func(json bool) []string {
			a := []string{"restore", "--catalog", cat, "--set-id", "S-JPARITY", "--target", restoreTarget(t),
				"--image", "1=" + destImageDir(dest, "S-JPARITY", 1),
				"--image", "2=" + destImageDir(dest, "S-JPARITY", 2)}
			if json {
				a = append(a, "--json")
			}
			return a
		}
		useRestoreFake(t)
		jsonStdout, _, jsonCode := runInProcess(t, args(true)...)
		_, humanStderr, humanCode := runInProcess(t, args(false)...)
		if humanCode != report.ExitOK || jsonCode != humanCode {
			t.Fatalf("restore exit codes: human=%d json=%d, want both 0", humanCode, jsonCode)
		}
		doc := decodeSingleJSONDoc(t, jsonStdout)
		h := extractNums(t, "restore summary",
			`restore: set \S+: \d+ image\(s\), selected (\d+) file\(s\) \([^)]*\), restored (\d+), skipped (\d+), failed (\d+),`,
			humanStderr, true)
		for jsonKey, humanN := range map[string]int{
			"files": h[0], "restored": h[1], "skipped": h[2], "failed": h[3],
		} {
			if j := jsonNum(t, doc, jsonKey); j != humanN {
				t.Errorf("restore work evidence: json %s=%d, human summary=%d",
					jsonKey, j, humanN)
			}
		}
		// selected counts FILES; restored counts every member written
		// (files + symlinks + dirs), so restored >= selected.
		if h[0] == 0 || h[1] < h[0] || h[3] != 0 {
			t.Errorf("restore fixture work: selected=%d restored=%d failed=%d, want >0/>=selected/0", h[0], h[1], h[3])
		}
	})
}
