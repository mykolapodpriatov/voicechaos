package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureRun runs the CLI with stdout and stderr redirected to temp files and
// returns (exit code, stdout, stderr). The CLI takes *os.File, so a pipe would
// need draining; files are simpler and the volumes here are tiny.
func captureRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "stdout")
	errPath := filepath.Join(dir, "stderr")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}

	code := run(args, out, errFile)

	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := errFile.Close(); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(errPath)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(stdout), string(stderr)
}

// checkFixture writes a scenario and a baseline saved from it, so `check`
// passes, plus a deliberately impossible baseline so `check` fails.
func checkFixture(t *testing.T) (scenario, goodBaseline, badBaseline string) {
	t.Helper()
	dir := t.TempDir()
	scenario = writeScenario(t)

	goodBaseline = filepath.Join(dir, "baseline.json")
	null := devNull(t)
	if code := run([]string{"baseline", "save", scenario, "--out", goodBaseline}, null, null); code != 0 {
		t.Fatalf("baseline save exited %d", code)
	}

	// A baseline of all zeros cannot be met by a run that measures anything.
	badBaseline = filepath.Join(dir, "impossible.json")
	if err := os.WriteFile(badBaseline, []byte(`{"callers":3,"seed":7,"aggregate":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return scenario, goodBaseline, badBaseline
}

func TestCheckTextIsUnchangedByDefault(t *testing.T) {
	scenario, good, _ := checkFixture(t)

	code, stdout, _ := captureRun(t, "check", scenario, "--baseline", good)

	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.HasPrefix(stdout, "check: PASS,") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestCheckJSONListsEveryMetricOnAPass(t *testing.T) {
	scenario, good, _ := checkFixture(t)

	code, stdout, _ := captureRun(t, "check", scenario, "--baseline", good, "--format", "json")

	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	var parsed struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Metric string `json:"metric"`
			OK     bool   `json:"ok"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if !parsed.OK || len(parsed.Checks) != 4 {
		t.Errorf("ok=%v checks=%d, want true/4", parsed.OK, len(parsed.Checks))
	}
}

func TestAMachineReadableFailureGoesToStdout(t *testing.T) {
	// Half a JSON document on stderr is not something a consumer can use; the
	// exit code already carries pass/fail.
	scenario, _, bad := checkFixture(t)

	code, stdout, _ := captureRun(t, "check", scenario, "--baseline", bad, "--format", "json")

	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if parsed["ok"] != false {
		t.Errorf("ok = %v, want false", parsed["ok"])
	}
}

func TestTextKeepsTheStdoutStderrSplit(t *testing.T) {
	scenario, _, bad := checkFixture(t)

	code, stdout, stderr := captureRun(t, "check", scenario, "--baseline", bad)

	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("failure detail leaked to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "check: FAIL,") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestGitHubFormatAnnotatesEachRegression(t *testing.T) {
	scenario, _, bad := checkFixture(t)

	code, stdout, _ := captureRun(t, "check", scenario, "--baseline", bad, "--format", "github")

	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stdout, "::error title=") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestSummaryAppendsRatherThanTruncating(t *testing.T) {
	// $GITHUB_STEP_SUMMARY is shared with every other step in the job.
	scenario, good, _ := checkFixture(t)
	summary := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(summary, []byte("## an earlier step\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, _ := captureRun(t, "check", scenario, "--baseline", good, "--summary", summary); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if code, _, _ := captureRun(t, "check", scenario, "--baseline", good, "--summary", summary); code != 0 {
		t.Fatalf("second run exited %d, want 0", code)
	}

	body, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "## an earlier step") {
		t.Error("the earlier step's summary was truncated")
	}
	if n := strings.Count(text, "| Metric |"); n != 2 {
		t.Errorf("summary tables = %d, want 2 (one appended per run)", n)
	}
}

func TestSummaryIsWrittenEvenInTextMode(t *testing.T) {
	scenario, good, _ := checkFixture(t)
	summary := filepath.Join(t.TempDir(), "summary.md")

	code, stdout, _ := captureRun(t, "check", scenario, "--baseline", good, "--summary", summary)

	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.HasPrefix(stdout, "check: PASS,") {
		t.Errorf("terminal output changed: %q", stdout)
	}
	body, err := os.ReadFile(summary)
	if err != nil {
		t.Fatalf("summary not written: %v", err)
	}
	if !strings.Contains(string(body), "| Metric |") {
		t.Errorf("summary = %q", body)
	}
}

func TestAnUnwritableSummaryPathFails(t *testing.T) {
	scenario, good, _ := checkFixture(t)
	summary := filepath.Join(t.TempDir(), "no-such-dir", "summary.md")

	code, _, stderr := captureRun(t, "check", scenario, "--baseline", good, "--summary", summary)

	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "--summary") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestAnUnknownFormatIsAUsageError(t *testing.T) {
	scenario, good, _ := checkFixture(t)

	for _, bad := range []string{"yaml", "md", "TEXT", ""} {
		code, _, _ := captureRun(t, "check", scenario, "--baseline", good, "--format", bad)
		if code != 2 {
			t.Errorf("--format %q: exit %d, want 2", bad, code)
		}
	}
}

func TestCompareSupportsTheSameFormats(t *testing.T) {
	null := devNull(t)
	dir := t.TempDir()
	scenario := writeScenario(t)
	reportA := filepath.Join(dir, "a.json")
	reportB := filepath.Join(dir, "b.json")
	for _, path := range []string{reportA, reportB} {
		if code := run([]string{"run", scenario, "--out", path}, null, null); code != 0 {
			t.Fatalf("run exited %d", code)
		}
	}

	code, stdout, _ := captureRun(t, "compare", reportA, reportB, "--format", "json")

	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	var parsed struct {
		Checks []struct{} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if len(parsed.Checks) != 4 {
		t.Errorf("checks = %d, want 4", len(parsed.Checks))
	}
}

func TestCompareTextSaysNoMetricWorsenedWithoutABudget(t *testing.T) {
	null := devNull(t)
	dir := t.TempDir()
	scenario := writeScenario(t)
	reportA := filepath.Join(dir, "a.json")
	reportB := filepath.Join(dir, "b.json")
	for _, path := range []string{reportA, reportB} {
		if code := run([]string{"run", scenario, "--out", path}, null, null); code != 0 {
			t.Fatalf("run exited %d", code)
		}
	}

	_, stdout, _ := captureRun(t, "compare", reportA, reportB)

	if !strings.Contains(stdout, "no metric worsened") {
		t.Errorf("stdout = %q", stdout)
	}
}
