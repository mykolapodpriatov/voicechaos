// Command voicechaos is a load + chaos test harness for real-time voice agents.
// It drives N concurrent synthetic voice sessions over a deterministically
// impaired transport, scripts millisecond-precise barge-ins, and measures
// barge-in correctness (time-to-stop, double-talk, stalls, dropped frames) with
// replayable scenarios and CI baselines.
//
// Subcommands:
//
//	voicechaos run     scenario.json [--loopback] [--out report.json]
//	voicechaos baseline save scenario.json --out baseline.json
//	voicechaos check   scenario.json --baseline baseline.json [--budget budget.json]
//	voicechaos report  report.json
//
// The default build runs the deterministic offline loopback path; a real
// WebSocket endpoint adapter exists (--endpoint) for live runs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"voicechaos/internal/baseline"
	"voicechaos/internal/config"
	"voicechaos/internal/runner"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the process exit code, so it is
// testable without spawning a process.
func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "run":
		return cmdRun(ctx, args[1:], stdout, stderr)
	case "baseline":
		return cmdBaseline(ctx, args[1:], stdout, stderr)
	case "check":
		return cmdCheck(ctx, args[1:], stdout, stderr)
	case "report":
		return cmdReport(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "voicechaos: unknown subcommand %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `voicechaos — load + chaos test harness for real-time voice agents

Usage:
  voicechaos run      <scenario.json> [--loopback] [--out report.json]
  voicechaos baseline save <scenario.json> --out <baseline.json>
  voicechaos check    <scenario.json> --baseline <baseline.json> [--budget <budget.json>]
  voicechaos report   <report.json>

Flags:
  --loopback   run the deterministic offline pipeline (default)
  --endpoint   (reserved) a wss:// endpoint for live runs
  --out        output file
  --baseline   baseline JSON for check
  --budget     budget JSON for check (defaults to a built-in budget)
`)
}

func cmdRun(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "write the report JSON to this file")
	_ = fs.Bool("loopback", true, "run the deterministic offline pipeline")
	endpoint := fs.String("endpoint", "", "reserved: wss:// endpoint for live runs")
	if code, ok := parseArgs(fs, args, stderr); !ok {
		return code
	}
	scenarioPath := fs.Arg(0)
	if scenarioPath == "" {
		fmt.Fprintln(stderr, "run: missing scenario path")
		return 2
	}
	if *endpoint != "" {
		fmt.Fprintln(stderr, "run: --endpoint live runs are not enabled in this build; use --loopback")
		return 2
	}
	sc, err := config.LoadScenario(scenarioPath)
	if err != nil {
		fmt.Fprintf(stderr, "run: %v\n", err)
		return 1
	}
	rn := &runner.Runner{}
	rep, err := rn.Run(ctx, sc)
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "run: %v\n", err)
		return 1
	}
	if err := emitJSON(*out, stdout, rep); err != nil {
		fmt.Fprintf(stderr, "run: %v\n", err)
		return 1
	}
	return 0
}

func cmdBaseline(ctx context.Context, args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] != "save" {
		fmt.Fprintln(stderr, "baseline: expected `baseline save <scenario.json> --out <baseline.json>`")
		return 2
	}
	fs := flag.NewFlagSet("baseline save", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "write the baseline JSON to this file")
	if code, ok := parseArgs(fs, args[1:], stderr); !ok {
		return code
	}
	scenarioPath := fs.Arg(0)
	if scenarioPath == "" {
		fmt.Fprintln(stderr, "baseline save: missing scenario path")
		return 2
	}
	sc, err := config.LoadScenario(scenarioPath)
	if err != nil {
		fmt.Fprintf(stderr, "baseline save: %v\n", err)
		return 1
	}
	rn := &runner.Runner{}
	rep, err := rn.Run(ctx, sc)
	if err != nil {
		fmt.Fprintf(stderr, "baseline save: %v\n", err)
		return 1
	}
	b := baseline.Baseline{Callers: sc.Callers, Seed: sc.Seed, Aggregate: rep.Aggregate}
	if *out == "" {
		if err := writeJSON(stdout, b); err != nil {
			fmt.Fprintf(stderr, "baseline save: %v\n", err)
			return 1
		}
		return 0
	}
	if err := baseline.Save(*out, b); err != nil {
		fmt.Fprintf(stderr, "baseline save: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "baseline written to %s\n", *out)
	return 0
}

func cmdCheck(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	basePath := fs.String("baseline", "", "baseline JSON to compare against")
	budgetPath := fs.String("budget", "", "budget JSON (defaults to a built-in budget)")
	if code, ok := parseArgs(fs, args, stderr); !ok {
		return code
	}
	scenarioPath := fs.Arg(0)
	if scenarioPath == "" || *basePath == "" {
		fmt.Fprintln(stderr, "check: usage: check <scenario.json> --baseline <baseline.json> [--budget <budget.json>]")
		return 2
	}
	sc, err := config.LoadScenario(scenarioPath)
	if err != nil {
		fmt.Fprintf(stderr, "check: %v\n", err)
		return 1
	}
	base, err := baseline.Load(*basePath)
	if err != nil {
		fmt.Fprintf(stderr, "check: %v\n", err)
		return 1
	}
	budget, err := config.LoadBudget(*budgetPath)
	if err != nil {
		fmt.Fprintf(stderr, "check: %v\n", err)
		return 1
	}
	rn := &runner.Runner{}
	rep, err := rn.Run(ctx, sc)
	if err != nil {
		fmt.Fprintf(stderr, "check: %v\n", err)
		return 1
	}
	result := baseline.Check(base, rep.Aggregate, budget)
	if result.OK {
		fmt.Fprintln(stdout, "check: PASS — all metrics within budget")
		return 0
	}
	fmt.Fprintln(stderr, "check: FAIL — budget exceeded:")
	for _, v := range result.Violations {
		fmt.Fprintf(stderr, "  - %s\n", v.Message)
	}
	return 1
}

func cmdReport(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "report: missing report path")
		return 2
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "report: %v\n", err)
		return 1
	}
	var rep runner.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		fmt.Fprintf(stderr, "report: parse: %v\n", err)
		return 1
	}
	printReport(stdout, rep)
	return 0
}

// printReport writes a concise human-readable summary of a report.
func printReport(w *os.File, rep runner.Report) {
	a := rep.Aggregate
	fmt.Fprintf(w, "voicechaos report — %d callers, seed %d\n", rep.Callers, rep.Seed)
	fmt.Fprintf(w, "  time-to-stop (ms): count=%d mean=%d p50=%d p95=%d max=%d\n",
		a.TimeToStop.Count, a.TimeToStop.Mean, a.TimeToStop.P50, a.TimeToStop.P95, a.TimeToStop.Max)
	fmt.Fprintf(w, "  double-talk  (ms): sum=%d mean=%d p95=%d\n", a.DoubleTalkMs.Sum, a.DoubleTalkMs.Mean, a.DoubleTalkMs.P95)
	fmt.Fprintf(w, "  stalls           : count=%d totalMs=%d\n", a.StallCount, a.StallMs)
	fmt.Fprintf(w, "  dropped frames   : %d\n", a.DroppedFrames)
}

// parseArgs parses fs, tolerating a positional argument placed AFTER flags
// (Go's flag package stops at the first non-flag token, so we reorder the single
// leading positional to the front). ok=false means the caller should return code.
func parseArgs(fs *flag.FlagSet, args []string, _ *os.File) (int, bool) {
	if err := fs.Parse(reorderPositional(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	return 0, true
}

// reorderPositional moves the first bare positional token (one not starting with
// "-" and not consumed as a flag value) to the end, so `cmd <pos> --flag v`
// parses the same as `cmd --flag v <pos>`. It assumes a single positional, which
// is true for every subcommand here.
func reorderPositional(args []string) []string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 0 && a[0] == '-' {
			// Skip a value following a non-boolean flag of the form "--flag value".
			if i+1 < len(args) && !isKnownBoolFlag(a) && !hasInlineValue(a) {
				i++
			}
			continue
		}
		// a is the positional; move it to the end.
		out := make([]string, 0, len(args))
		out = append(out, args[:i]...)
		out = append(out, args[i+1:]...)
		out = append(out, a)
		return out
	}
	return args
}

// isKnownBoolFlag reports whether a token names one of the boolean flags used by
// the CLI (which take no value).
func isKnownBoolFlag(tok string) bool {
	name := tok
	for len(name) > 0 && name[0] == '-' {
		name = name[1:]
	}
	return name == "loopback"
}

// hasInlineValue reports whether a flag token already carries its value as
// "--flag=value".
func hasInlineValue(tok string) bool {
	for i := 0; i < len(tok); i++ {
		if tok[i] == '=' {
			return true
		}
	}
	return false
}

// emitJSON writes v to path (or stdout when path is empty).
func emitJSON(path string, stdout *os.File, v any) error {
	if path == "" {
		return writeJSON(stdout, v)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func writeJSON(w *os.File, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
