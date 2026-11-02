// Command claude-bouncer is a Claude Code PreToolUse hook that holds tool
// calls of other sessions so that one "patron" session keeps the quota it
// needs. It only reads files and sleeps; it never writes anything.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const version = "0.2.0"

const usageText = `claude-bouncer holds Claude Code tool calls to keep quota for a patron session.

Hook (reads the PreToolUse JSON on stdin; on any problem the call runs, silently):
  claude-bouncer -config FILE -usage FILE [-poll 1m]

Diagnose without sleeping:
  claude-bouncer -config FILE -usage FILE -dry-run [-session NAME] [-now "YYYY-MM-DD HH:MM"]

Options:
`

type options struct {
	configPath  string
	usagePath   string
	poll        time.Duration
	dryRun      bool
	session     string
	now         string
	showVersion bool
}

// system bundles every outside effect so tests can replace them.
type system struct {
	stdin           io.Reader
	stdinIsTerminal bool
	stdout          io.Writer
	stderr          io.Writer
	now             func() time.Time
	sleep           func(time.Duration)
	readFile        func(string) ([]byte, error)
	glob            func(string) ([]string, error)
	getenv          func(string) string
	userHomeDir     func() (string, error)
	alive           func(int) bool
}

func main() {
	terminal := false
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		terminal = true
	}
	sys := &system{
		stdin:           os.Stdin,
		stdinIsTerminal: terminal,
		stdout:          os.Stdout,
		stderr:          os.Stderr,
		now:             time.Now,
		sleep:           time.Sleep,
		readFile:        readFileRetry,
		glob:            filepath.Glob,
		getenv:          os.Getenv,
		userHomeDir:     os.UserHomeDir,
		alive:           processAlive,
	}
	os.Exit(run(os.Args[1:], sys))
}

// run returns the process exit code. It is never 2: for a PreToolUse hook,
// exit code 2 would block the tool call instead of merely delaying it.
func run(args []string, sys *system) int {
	opts, err := parseOptions(args, sys.stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(sys.stderr, "claude-bouncer:", err)
		return 1
	}
	if opts.showVersion {
		fmt.Fprintf(sys.stdout, "claude-bouncer %s\n", version)
		return 0
	}
	if opts.dryRun {
		return dryRun(opts, sys)
	}
	return hook(opts, sys)
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	opts := options{poll: time.Minute}
	fs := flag.NewFlagSet("claude-bouncer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.configPath, "config", "", "policy file: patron, start, end, daily_reserve (required)")
	fs.StringVar(&opts.usagePath, "usage", "", "usage file published by continue-claude -export (required)")
	fs.DurationVar(&opts.poll, "poll", opts.poll, "how often a held call re-reads both files")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "print the decision instead of sleeping")
	fs.StringVar(&opts.session, "session", "", "with -dry-run: evaluate as this session name instead of reading stdin")
	fs.StringVar(&opts.now, "now", "", `with -dry-run: evaluate at this local time, "YYYY-MM-DD HH:MM"`)
	fs.BoolVar(&opts.showVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.showVersion {
		return opts, nil
	}
	if opts.configPath == "" {
		return opts, errors.New("missing required -config FILE")
	}
	if opts.usagePath == "" {
		return opts, errors.New("missing required -usage FILE")
	}
	if opts.poll <= 0 {
		return opts, errors.New("-poll must be positive")
	}
	if !opts.dryRun && (opts.session != "" || opts.now != "") {
		return opts, errors.New("-session and -now are only allowed with -dry-run")
	}
	return opts, nil
}

// hook is the PreToolUse path: return at once when the call may run, or sleep
// until it may. It fails open and quietly: when anything goes wrong (a missing
// or renamed policy file, an unreachable share, unreadable input) the call
// simply runs, as it would without the hook, and nothing is printed. Renaming
// the policy file is therefore the way to switch the bouncer off.
func hook(opts options, sys *system) int {
	if sys.stdinIsTerminal {
		// Only reachable when a person runs the command by hand.
		fmt.Fprintln(sys.stderr, "claude-bouncer: expects Claude Code's PreToolUse JSON on stdin; use -dry-run -session NAME to try it by hand")
		return 1
	}
	in, err := readHookInput(sys.stdin)
	if err != nil {
		return 0
	}
	matcher := newPatronMatcher(in, sys)

	cfg, err := loadConfig(opts.configPath, sys)
	if err != nil {
		return 0
	}
	start := sys.now()
	v := evaluate(cfg, start, matcher, opts.usagePath, sys)
	if !v.Hold {
		return 0
	}

	if !holdUntilAllowed(opts, sys, matcher, v) {
		return 0 // the Claude Code process is gone; nobody is waiting
	}
	writeReleaseMessage(sys, v, start, sys.now())
	return 0
}

// holdUntilAllowed sleeps while the call stays held, re-reading both files
// every poll interval, so that editing or renaming the policy takes effect
// within one interval. A held call is released as soon as the policy no
// longer holds it or can no longer be read. It returns false when the owning
// Claude Code process has exited in the meantime.
func holdUntilAllowed(opts options, sys *system, matcher *patronMatcher, v verdict) bool {
	pid, _ := strconv.Atoi(strings.TrimSpace(sys.getenv("CLAUDE_PID")))
	for {
		wait := v.Until.Sub(sys.now())
		if wait > opts.poll {
			wait = opts.poll
		}
		if wait > 0 {
			sys.sleep(wait)
		}
		if pid > 0 && !sys.alive(pid) {
			return false
		}

		now := sys.now()
		cfg, err := loadConfig(opts.configPath, sys)
		if err != nil {
			return true
		}
		next := evaluate(cfg, now, matcher, opts.usagePath, sys)
		if !next.Hold {
			return true
		}
		v = next
	}
}

func evaluate(cfg config, now time.Time, matcher *patronMatcher, usagePath string, sys *system) verdict {
	return decide(cfg, now,
		func() bool { return matcher.Matches(cfg.Patron) },
		func() (weekly, error) { return loadWeekly(usagePath, now, sys) })
}

// writeReleaseMessage tells the user, and Claude, that the call was held, so
// a session that resumes hours later knows that time has passed.
func writeReleaseMessage(sys *system, first verdict, from, to time.Time) {
	held := fmt.Sprintf("%s to %s", formatMinute(from, to), formatMinute(to, from))
	out := map[string]any{
		"systemMessage": fmt.Sprintf("claude-bouncer held this call from %s (%s)", held, first.Reason),
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse",
			"additionalContext": fmt.Sprintf(
				"claude-bouncer held this tool call from %s to %s local time (%s). Time has passed since the call was made; refresh anything time-sensitive before relying on it.",
				from.Format("2006-01-02 15:04"), to.Format("2006-01-02 15:04"), first.Reason),
		},
	}
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Fprintln(sys.stdout, string(data))
}

func dryRun(opts options, sys *system) int {
	now := sys.now()
	if opts.now != "" {
		t, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(opts.now), time.Local)
		if err != nil {
			fmt.Fprintf(sys.stderr, "claude-bouncer: -now %q is not \"YYYY-MM-DD HH:MM\"\n", opts.now)
			return 1
		}
		now = t
	}

	var matcher *patronMatcher
	switch {
	case opts.session != "":
		matcher = fixedPatronMatcher(opts.session)
	case !sys.stdinIsTerminal:
		in, err := readHookInput(sys.stdin)
		if err != nil {
			fmt.Fprintln(sys.stderr, "claude-bouncer:", err)
			return 1
		}
		matcher = newPatronMatcher(in, sys)
	default:
		matcher = fixedPatronMatcher("")
	}

	w := sys.stdout
	fmt.Fprintf(w, "now       %s\n", now.Format("2006-01-02 15:04"))
	cfg, err := loadConfig(opts.configPath, sys)
	if err != nil {
		fmt.Fprintf(w, "policy    unreadable: %v\n", err)
		fmt.Fprintln(w, "decision  run: without a readable policy the bouncer is off")
		return 0
	}
	wk, werr := loadWeekly(opts.usagePath, now, sys)
	v := decide(cfg, now,
		func() bool { return matcher.Matches(cfg.Patron) },
		func() (weekly, error) { return wk, werr })

	if v.InWindow {
		fmt.Fprintf(w, "window    inside %s-%s Mon-Fri, until %s\n", cfg.Start, cfg.End, formatMinute(v.Window.End, now))
	} else {
		fmt.Fprintf(w, "window    outside %s-%s Mon-Fri, next opens %s %s\n", cfg.Start, cfg.End, v.Window.Start.Weekday().String()[:3], formatMinute(v.Window.Start, now))
	}
	name := matcher.Name()
	if name == "" {
		name = "(unknown)"
	}
	fmt.Fprintf(w, "session   %s, patron %q: %s\n", name, cfg.Patron, yesNo(matcher.Matches(cfg.Patron)))
	if werr != nil {
		fmt.Fprintf(w, "weekly    unavailable: %v\n", werr)
	} else {
		note := ""
		if wk.Rolled {
			note = " (file predates the last reset; counted as 0%)"
		}
		fmt.Fprintf(w, "weekly    used %s%%, left %s%%, resets %s%s\n", formatPercent(wk.Used), formatPercent(wk.Remain()), formatMinute(wk.Reset, now), note)
		windows := cfg.windowsBefore(now, wk.Reset)
		fmt.Fprintf(w, "reserve   %s%% x %d window(s) before reset = %s%%\n", formatPercent(cfg.DailyReserve), windows, formatPercent(round1(cfg.DailyReserve*float64(windows))))
	}
	switch {
	case v.Hold:
		fmt.Fprintf(w, "decision  hold until %s: %s\n", formatMinute(v.Until, now), v.Reason)
	case v.Warning != "":
		fmt.Fprintf(w, "decision  run (%s)\n", v.Warning)
	case v.InWindow:
		fmt.Fprintln(w, "decision  run: the patron owns this window")
	case v.Reserve <= 0:
		fmt.Fprintln(w, "decision  run: nothing to reserve before the weekly reset")
	default:
		fmt.Fprintln(w, "decision  run: the weekly limit left is above the reserve")
	}
	return 0
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func readHookInput(r io.Reader) (hookInput, error) {
	data, err := io.ReadAll(io.LimitReader(r, 16<<20))
	if err != nil {
		return hookInput{}, fmt.Errorf("read hook input: %w", err)
	}
	var in hookInput
	if err := json.Unmarshal(data, &in); err != nil {
		return hookInput{}, fmt.Errorf("parse hook input: %w", err)
	}
	return in, nil
}

func loadConfig(path string, sys *system) (config, error) {
	expanded, err := expandUserPath(path, sys)
	if err != nil {
		return config{}, err
	}
	data, err := sys.readFile(expanded)
	if err != nil {
		return config{}, fmt.Errorf("read config: %w", err)
	}
	cfg, err := parseConfig(data)
	if err != nil {
		return config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func loadWeekly(path string, now time.Time, sys *system) (weekly, error) {
	expanded, err := expandUserPath(path, sys)
	if err != nil {
		return weekly{}, err
	}
	data, err := sys.readFile(expanded)
	if err != nil {
		return weekly{}, fmt.Errorf("read usage file: %w", err)
	}
	return parseWeekly(data, now)
}

// readFileRetry reads a file, retrying briefly on errors other than "not
// found": the usage file is replaced by rename, which can make a reader on a
// network share see a transient sharing violation.
func readFileRetry(path string) ([]byte, error) {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		var data []byte
		data, err = os.ReadFile(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return data, err
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	return nil, err
}

func expandUserPath(path string, sys *system) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path, nil
	}
	home, err := sys.userHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, filepath.FromSlash(path[2:])), nil
}
