package baseline

import (
	"encoding/json"
	"strings"
	"testing"

	"voicechaos/internal/metrics"
)

// passing builds a result where every metric is comfortably inside budget.
func passing() CheckResult {
	base := Baseline{Aggregate: metrics.Aggregate{
		TimeToStop:   metrics.Summary{P95: 100},
		DoubleTalkMs: metrics.Summary{Sum: 200},
		StallMs:      0,
	}}
	current := metrics.Aggregate{
		TimeToStop:   metrics.Summary{P95: 90},
		DoubleTalkMs: metrics.Summary{Sum: 180},
		StallMs:      0,
	}
	return Check(base, current, DefaultBudget)
}

// failing builds a result where time-to-stop and stalls both regress.
func failing() CheckResult {
	base := Baseline{Aggregate: metrics.Aggregate{
		TimeToStop:   metrics.Summary{P95: 100},
		DoubleTalkMs: metrics.Summary{Sum: 200},
		StallMs:      0,
	}}
	current := metrics.Aggregate{
		TimeToStop:   metrics.Summary{P95: 400},
		DoubleTalkMs: metrics.Summary{Sum: 180},
		StallMs:      50,
	}
	return Check(base, current, DefaultBudget)
}

// ---- Check now records every metric, not only the failures -----------------

func TestEveryConstraintIsRecordedEvenWhenItPasses(t *testing.T) {
	res := passing()

	if !res.OK {
		t.Fatalf("expected a passing result, got violations: %+v", res.Violations)
	}
	got := make([]string, 0, len(res.Checks))
	for _, c := range res.Checks {
		got = append(got, c.Metric)
		if !c.OK {
			t.Errorf("%s reported not-OK in a passing result", c.Metric)
		}
		if c.Message != "" {
			t.Errorf("%s carries a failure message while passing: %q", c.Metric, c.Message)
		}
	}
	want := []string{"time_to_stop_p95_ms", "double_talk_total_ms", "stall_total_ms", "dropped_frames"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("metric order = %v, want %v", got, want)
	}
}

func TestDeltaIsSignedAgainstTheBaseline(t *testing.T) {
	res := failing()

	byName := map[string]MetricCheck{}
	for _, c := range res.Checks {
		byName[c.Metric] = c
	}
	if got := byName["time_to_stop_p95_ms"].Delta; got != 300 {
		t.Errorf("time-to-stop delta = %v, want +300", got)
	}
	// An improvement has to read as negative, not as a bare smaller number.
	if got := byName["double_talk_total_ms"].Delta; got != -20 {
		t.Errorf("double-talk delta = %v, want -20", got)
	}
}

func TestViolationsRemainTheFailingSubset(t *testing.T) {
	res := failing()

	if len(res.Checks) != 4 {
		t.Fatalf("checks = %d, want 4", len(res.Checks))
	}
	if len(res.Violations) != 2 {
		t.Fatalf("violations = %d, want 2 (time-to-stop and stalls)", len(res.Violations))
	}
	for _, v := range res.Violations {
		if v.Message == "" {
			t.Errorf("violation %s has no message", v.Metric)
		}
	}
}

// ---- ParseFormat -----------------------------------------------------------

func TestParseFormatAcceptsEveryDocumentedName(t *testing.T) {
	for _, name := range []string{"text", "json", "markdown", "github"} {
		if _, err := ParseFormat(name); err != nil {
			t.Errorf("ParseFormat(%q) = %v, want nil", name, err)
		}
	}
}

func TestParseFormatRejectsAnythingElse(t *testing.T) {
	// A typo in a CI job must not quietly turn a machine-readable report into
	// prose the next step cannot parse.
	for _, name := range []string{"", "TEXT", "md", "yaml", "term"} {
		if _, err := ParseFormat(name); err == nil {
			t.Errorf("ParseFormat(%q) = nil error, want a rejection", name)
		}
	}
}

// ---- text ------------------------------------------------------------------

func TestTextPassAndFailAreStable(t *testing.T) {
	if got := RenderText(passing(), CheckLabels); got != "check: PASS, all metrics within budget\n" {
		t.Errorf("pass text = %q", got)
	}
	got := RenderText(failing(), CheckLabels)
	if !strings.HasPrefix(got, "check: FAIL, budget exceeded:\n") {
		t.Errorf("fail text does not start with the FAIL line: %q", got)
	}
	if strings.Count(got, "\n  - ") != 2 {
		t.Errorf("fail text should carry one bullet per violation: %q", got)
	}
}

func TestTextUsesTheCallersLabels(t *testing.T) {
	labels := Labels{Command: "compare", PassNote: "no metric worsened", FailNote: "regression"}

	if got := RenderText(passing(), labels); got != "compare: PASS, no metric worsened\n" {
		t.Errorf("pass text = %q", got)
	}
	if got := RenderText(failing(), labels); !strings.HasPrefix(got, "compare: FAIL, regression:\n") {
		t.Errorf("fail text = %q", got)
	}
}

// ---- json ------------------------------------------------------------------

func TestJSONListsEveryMetricOnAPass(t *testing.T) {
	out, err := RenderJSON(passing())
	if err != nil {
		t.Fatal(err)
	}

	var parsed CheckResult
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if !parsed.OK {
		t.Error("ok = false on a passing result")
	}
	if len(parsed.Checks) != 4 {
		t.Errorf("checks = %d, want 4; a passing run must be as inspectable as a failing one", len(parsed.Checks))
	}
}

func TestJSONNeverEmitsNullArrays(t *testing.T) {
	// A consumer indexing into checks should not have to special-case null.
	out, err := RenderJSON(CheckResult{OK: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "null") {
		t.Errorf("output contains null: %s", out)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed["checks"].([]any); !ok {
		t.Errorf("checks is not an array: %v", parsed["checks"])
	}
}

func TestJSONIsStableAcrossRuns(t *testing.T) {
	first, err := RenderJSON(failing())
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderJSON(failing())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("two renders of the same result differ")
	}
}

// ---- markdown --------------------------------------------------------------

func TestMarkdownIsATableOfEveryMetric(t *testing.T) {
	got := RenderMarkdown(failing(), CheckLabels)

	if !strings.Contains(got, "### voicechaos check: FAIL") {
		t.Errorf("missing heading: %s", got)
	}
	if !strings.Contains(got, "| Metric | Baseline | Current | Delta | Budget | Result |") {
		t.Errorf("missing table header: %s", got)
	}
	for _, metric := range []string{"time_to_stop_p95_ms", "double_talk_total_ms", "stall_total_ms", "dropped_frames"} {
		if !strings.Contains(got, "`"+metric+"`") {
			t.Errorf("missing row for %s: %s", metric, got)
		}
	}
	if !strings.Contains(got, "+300") {
		t.Errorf("a regression's delta should carry an explicit sign: %s", got)
	}
	if !strings.Contains(got, "-20") {
		t.Errorf("an improvement should read as negative: %s", got)
	}
}

func TestMarkdownOnAPassStillListsEveryMetric(t *testing.T) {
	got := RenderMarkdown(passing(), CheckLabels)

	if !strings.Contains(got, "### voicechaos check: PASS") {
		t.Errorf("missing heading: %s", got)
	}
	if n := strings.Count(got, "| pass |"); n != 4 {
		t.Errorf("passing rows = %d, want 4: %s", n, got)
	}
	if strings.Contains(got, "FAIL") {
		t.Errorf("a passing table should not contain FAIL: %s", got)
	}
}

// ---- github ----------------------------------------------------------------

func TestGitHubEmitsOneErrorPerViolation(t *testing.T) {
	got := RenderGitHub(failing(), CheckLabels)

	if n := strings.Count(got, "::error title="); n != 2 {
		t.Errorf("annotations = %d, want 2: %s", n, got)
	}
	if strings.Contains(got, "::notice") {
		t.Errorf("a failing result should not emit a notice: %s", got)
	}
}

func TestGitHubEmitsANoticeOnAPass(t *testing.T) {
	got := RenderGitHub(passing(), CheckLabels)

	if !strings.HasPrefix(got, "::notice title=") {
		t.Errorf("pass output = %q", got)
	}
	if strings.Contains(got, "::error") {
		t.Errorf("a passing result should not emit an error: %s", got)
	}
}

func TestGitHubEscapesWorkflowCommandMetacharacters(t *testing.T) {
	// An unescaped newline or colon would truncate or split the annotation.
	res := CheckResult{
		OK: false,
		Violations: []Violation{{
			Metric:  "a:b,c",
			Message: "line one\nline two 100% of the time",
		}},
	}

	got := RenderGitHub(res, CheckLabels)

	if strings.Count(got, "\n") != 1 {
		t.Errorf("annotation spans more than one line: %q", got)
	}
	if !strings.Contains(got, "%0A") {
		t.Errorf("newline not escaped: %q", got)
	}
	if !strings.Contains(got, "%25") {
		t.Errorf("percent not escaped: %q", got)
	}
	if !strings.Contains(got, "a%3Ab%2Cc") {
		t.Errorf("colon/comma not escaped in the title: %q", got)
	}
}

// ---- Render dispatch -------------------------------------------------------

func TestRenderDispatchesToEachFormat(t *testing.T) {
	res := failing()
	for _, tc := range []struct {
		format Format
		want   string
	}{
		{FormatText, "check: FAIL"},
		{FormatJSON, `"checks"`},
		{FormatMarkdown, "| Metric |"},
		{FormatGitHub, "::error title="},
	} {
		got, err := Render(res, tc.format, CheckLabels)
		if err != nil {
			t.Fatalf("Render(%s) = %v", tc.format, err)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("Render(%s) missing %q: %s", tc.format, tc.want, got)
		}
	}
}

func TestRenderRejectsAnUnknownFormat(t *testing.T) {
	if _, err := Render(failing(), Format("yaml"), CheckLabels); err == nil {
		t.Error("Render with an unknown format returned no error")
	}
}
