package baseline

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Format is an output shape for a CheckResult.
type Format string

const (
	// FormatText is the default: the terminal lines the CLI has always printed.
	FormatText Format = "text"
	// FormatJSON is the full result, every metric, for a dashboard or a bot.
	FormatJSON Format = "json"
	// FormatMarkdown is a table suited to $GITHUB_STEP_SUMMARY.
	FormatMarkdown Format = "markdown"
	// FormatGitHub is workflow commands, so each regression is annotated on
	// the run rather than buried in a collapsed log.
	FormatGitHub Format = "github"
)

// Formats lists every accepted format, in the order they are documented.
var Formats = []Format{FormatText, FormatJSON, FormatMarkdown, FormatGitHub}

// ParseFormat validates a --format value.
//
// An unknown value is an error rather than a silent fall back to text: a typo
// in a CI job must not quietly turn a machine-readable report into prose that
// the next step cannot parse.
func ParseFormat(s string) (Format, error) {
	for _, f := range Formats {
		if string(f) == s {
			return f, nil
		}
	}
	names := make([]string, 0, len(Formats))
	for _, f := range Formats {
		names = append(names, string(f))
	}
	return "", fmt.Errorf("unknown format %q; expected one of: %s", s, strings.Join(names, ", "))
}

// Labels carry the prose around a result, so `check` and `compare` can share
// every renderer while still saying what they mean. `compare` without a budget
// is asserting that no metric worsened; `check` is asserting a budget held, and
// telling a reader the wrong one is worse than telling them nothing.
type Labels struct {
	// Command is the subcommand name that prefixes the text summary.
	Command string
	// PassNote completes the PASS line.
	PassNote string
	// FailNote completes the FAIL line.
	FailNote string
}

// CheckLabels is what `check` reports under.
var CheckLabels = Labels{Command: "check", PassNote: "all metrics within budget", FailNote: "budget exceeded"}

// RenderText returns the human-readable summary: a PASS line, or a FAIL line
// followed by one bullet per violation.
func RenderText(res CheckResult, l Labels) string {
	if res.OK {
		return fmt.Sprintf("%s: PASS, %s\n", l.Command, l.PassNote)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: FAIL, %s:\n", l.Command, l.FailNote)
	for _, v := range res.Violations {
		fmt.Fprintf(&b, "  - %s\n", v.Message)
	}
	return b.String()
}

// RenderJSON returns the whole result, including the metrics that passed.
func RenderJSON(res CheckResult) (string, error) {
	// Never null: a consumer indexing into checks should not have to special
	// case an empty result. Violations keeps its omitempty, so the shape a
	// existing consumer already parses is unchanged.
	if res.Checks == nil {
		res.Checks = []MetricCheck{}
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

// RenderMarkdown returns a table of every metric with its budget and delta,
// suited to $GITHUB_STEP_SUMMARY.
func RenderMarkdown(res CheckResult, l Labels) string {
	var b strings.Builder
	if res.OK {
		fmt.Fprintf(&b, "### voicechaos %s: PASS\n\n", l.Command)
	} else {
		fmt.Fprintf(&b, "### voicechaos %s: FAIL\n\n", l.Command)
	}
	b.WriteString("| Metric | Baseline | Current | Delta | Budget | Result |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | :---: |\n")
	for _, c := range res.Checks {
		result := "pass"
		if !c.OK {
			result = "**FAIL**"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s |\n",
			c.Metric, num(c.Baseline), num(c.Current), signed(c.Delta), num(c.Limit), result)
	}
	if !res.OK {
		b.WriteString("\n")
		for _, v := range res.Violations {
			fmt.Fprintf(&b, "- %s\n", v.Message)
		}
	}
	return b.String()
}

// RenderGitHub returns GitHub Actions workflow commands: one error annotation
// per violation, or a single notice when everything held.
//
// Annotation text is escaped per the workflow-command rules, so a message
// containing a newline or a colon cannot truncate or split the annotation.
func RenderGitHub(res CheckResult, l Labels) string {
	if res.OK {
		return "::notice title=" + escapeProperty("voicechaos "+l.Command) + "::" +
			escapeData(l.PassNote) + "\n"
	}
	var b strings.Builder
	for _, v := range res.Violations {
		fmt.Fprintf(&b, "::error title=%s::%s\n",
			escapeProperty("voicechaos "+l.Command+" "+v.Metric), escapeData(v.Message))
	}
	return b.String()
}

// Render dispatches on format. It returns an error only for FormatJSON, which
// can fail to marshal.
func Render(res CheckResult, f Format, l Labels) (string, error) {
	switch f {
	case FormatJSON:
		return RenderJSON(res)
	case FormatMarkdown:
		return RenderMarkdown(res, l), nil
	case FormatGitHub:
		return RenderGitHub(res, l), nil
	case FormatText:
		return RenderText(res, l), nil
	default:
		return "", fmt.Errorf("unknown format %q", f)
	}
}

// num formats a metric value: whole numbers without a decimal tail, since every
// metric here is milliseconds or a frame count and "40" reads better than
// "40.0", but a computed budget like 44.0 keeps its precision when it has any.
func num(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.1f", v)
}

// signed formats a delta with an explicit sign, so an improvement is visibly
// an improvement rather than a number the reader has to compare by eye.
func signed(v float64) string {
	if v > 0 {
		return "+" + num(v)
	}
	return num(v)
}

// escapeData escapes a workflow-command message per GitHub's rules.
func escapeData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

// escapeProperty escapes a workflow-command property value, which additionally
// may not contain a colon or a comma.
func escapeProperty(s string) string {
	return strings.NewReplacer(
		"%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C",
	).Replace(s)
}
