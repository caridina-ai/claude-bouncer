package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var taipei = time.FixedZone("CST", 8*3600)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, taipei)
	if err != nil {
		panic(err)
	}
	return t
}

func testConfig() config {
	return config{Patron: "trader", Start: clock{8, 30}, End: clock{13, 30}, DailyReserve: 10, FiveHourThreshold: 95}
}

// --- config -----------------------------------------------------------------

func TestParseConfig(t *testing.T) {
	// A policy written for an older version has no five_hour_threshold.
	cfg, err := parseConfig([]byte("\xef\xbb\xbf" + `{"patron":" trader ","start":"8:30","end":"13:30","daily_reserve":12.5}`))
	if err != nil {
		t.Fatal(err)
	}
	want := config{Patron: "trader", Start: clock{8, 30}, End: clock{13, 30}, DailyReserve: 12.5, FiveHourThreshold: 95, ThresholdDefault: true}
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}

	cfg, err = parseConfig([]byte(`{"patron":"trader","start":"8:30","end":"13:30","daily_reserve":12.5,"five_hour_threshold":80}`))
	if err != nil || cfg.FiveHourThreshold != 80 || cfg.ThresholdDefault {
		t.Fatalf("explicit threshold: got %+v %v", cfg, err)
	}
}

func TestParseConfigRejects(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":   `{"patron":"a","start":"8:30","end":"13:30","daily_reserve":10,"extra":1}`,
		"missing field":   `{"patron":"a","start":"8:30","end":"13:30"}`,
		"empty patron":    `{"patron":" ","start":"8:30","end":"13:30","daily_reserve":10}`,
		"bad start":       `{"patron":"a","start":"8.30","end":"13:30","daily_reserve":10}`,
		"bad hour":        `{"patron":"a","start":"24:00","end":"13:30","daily_reserve":10}`,
		"same times":      `{"patron":"a","start":"8:30","end":"08:30","daily_reserve":10}`,
		"reserve > 100":   `{"patron":"a","start":"8:30","end":"13:30","daily_reserve":101}`,
		"negative":        `{"patron":"a","start":"8:30","end":"13:30","daily_reserve":-1}`,
		"trailing data":   `{"patron":"a","start":"8:30","end":"13:30","daily_reserve":10} {}`,
		"threshold > 100": `{"patron":"a","start":"8:30","end":"13:30","daily_reserve":10,"five_hour_threshold":101}`,
		"threshold < 0":   `{"patron":"a","start":"8:30","end":"13:30","daily_reserve":10,"five_hour_threshold":-5}`,
		"threshold text":  `{"patron":"a","start":"8:30","end":"13:30","daily_reserve":10,"five_hour_threshold":"95"}`,
		"not json":        `patron=a`,
	} {
		if _, err := parseConfig([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestWindows(t *testing.T) {
	cfg := testConfig()
	if _, ok := cfg.currentWindow(at("2026-11-03 08:29")); ok {
		t.Error("8:29 should be outside")
	}
	if w, ok := cfg.currentWindow(at("2026-11-03 08:30")); !ok || !w.End.Equal(at("2026-11-03 13:30")) {
		t.Errorf("8:30 should be inside until 13:30, got %v %v", w, ok)
	}
	if _, ok := cfg.currentWindow(at("2026-11-03 13:30")); ok {
		t.Error("13:30 should be outside")
	}
	if w := cfg.nextWindow(at("2026-11-03 13:30")); !w.Start.Equal(at("2026-11-04 08:30")) {
		t.Errorf("next window after close: %v", w.Start)
	}
	if w := cfg.nextWindow(at("2026-11-03 07:00")); !w.Start.Equal(at("2026-11-03 08:30")) {
		t.Errorf("next window before open: %v", w.Start)
	}

	// Windows still ahead that start before the reset, a straddling one included.
	for _, c := range []struct {
		now, reset string
		want       int
	}{
		{"2026-11-03 07:00", "2026-11-05 15:00", 3},
		{"2026-11-03 14:00", "2026-11-05 15:00", 2},
		{"2026-11-03 14:00", "2026-11-05 08:30", 1},
		{"2026-11-03 14:00", "2026-11-05 08:31", 2},
		{"2026-11-05 14:00", "2026-11-05 15:00", 0},
	} {
		if got := cfg.windowsBefore(at(c.now), at(c.reset)); got != c.want {
			t.Errorf("windowsBefore(%s, %s) = %d, want %d", c.now, c.reset, got, c.want)
		}
	}
}

func TestOvernightWindow(t *testing.T) {
	cfg := config{Patron: "p", Start: clock{22, 0}, End: clock{3, 0}, DailyReserve: 5}
	w, ok := cfg.currentWindow(at("2026-11-03 01:00"))
	if !ok || !w.Start.Equal(at("2026-11-02 22:00")) || !w.End.Equal(at("2026-11-03 03:00")) {
		t.Fatalf("01:00 should be inside last night's window, got %v %v", w, ok)
	}
	if _, ok := cfg.currentWindow(at("2026-11-03 12:00")); ok {
		t.Error("noon should be outside")
	}
}

func TestWeekends(t *testing.T) {
	if at("2026-11-07 12:00").Weekday() != time.Saturday {
		t.Fatal("calendar assumption broken: 2026-11-07 should be a Saturday")
	}
	cfg := testConfig()
	for _, s := range []string{"2026-11-07 09:00", "2026-11-08 09:00"} {
		if _, ok := cfg.currentWindow(at(s)); ok {
			t.Errorf("%s is a weekend: no exclusive window", s)
		}
	}
	w := cfg.nextWindow(at("2026-11-06 14:00"))
	if !w.Start.Equal(at("2026-11-09 08:30")) || !w.End.Equal(at("2026-11-09 13:30")) {
		t.Errorf("after Friday's close the next window is Monday's, got %v", w)
	}
	for _, c := range []struct {
		now, reset string
		want       int
	}{
		{"2026-11-06 14:00", "2026-11-11 15:00", 3}, // Mon, Tue, Wed
		{"2026-11-05 07:00", "2026-11-10 15:00", 4}, // Thu, Fri, Mon, Tue
		{"2026-11-07 10:00", "2026-11-09 08:00", 0}, // nothing before Monday's open
	} {
		if got := cfg.windowsBefore(at(c.now), at(c.reset)); got != c.want {
			t.Errorf("windowsBefore(%s, %s) = %d, want %d", c.now, c.reset, got, c.want)
		}
	}

	// An overnight window belongs to the weekday it starts on.
	night := config{Patron: "p", Start: clock{22, 0}, End: clock{3, 0}, DailyReserve: 5}
	for s, want := range map[string]bool{
		"2026-11-07 01:00": true,  // Friday night's window
		"2026-11-08 01:00": false, // Saturday night has none
		"2026-11-09 01:00": false, // nor Sunday night
		"2026-11-10 01:00": true,  // Monday night's window
	} {
		if _, ok := night.currentWindow(at(s)); ok != want {
			t.Errorf("overnight window at %s: inside = %v, want %v", s, ok, want)
		}
	}
}

func TestDecideWeekend(t *testing.T) {
	cfg := testConfig()
	// Saturday morning: no window, so only the reserve rule applies.
	if v := decide(cfg, at("2026-11-07 09:00"), is(false), weeklyOf(10, "2026-11-11 15:00"), false); v.Hold || v.InWindow {
		t.Errorf("weekend with plenty left should run: %+v", v)
	}
	// Mon, Tue, Wed before the reset: 30% kept, 25% left.
	v := decide(cfg, at("2026-11-07 09:00"), is(false), weeklyOf(75, "2026-11-11 15:00"), false)
	if !v.Hold || v.Windows != 3 || !v.Until.Equal(at("2026-11-09 13:30")) {
		t.Errorf("weekend, short: want hold until Monday 13:30 with 3 windows, got %+v", v)
	}
	v = decide(cfg, at("2026-11-07 09:00"), is(true), weeklyOf(75, "2026-11-11 15:00"), false)
	if !v.Hold || !v.Until.Equal(at("2026-11-09 08:30")) {
		t.Errorf("weekend, short, patron: want hold until Monday 8:30, got %+v", v)
	}
}

// --- usage ------------------------------------------------------------------

func usageJSON(used float64, reset time.Time) []byte {
	return []byte(fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":3,"resets_at":1},"seven_day":{"used_percentage":%g,"resets_at":%d}}}`, used, reset.Unix()))
}

// usageBoth publishes both limits.
func usageBoth(weeklyUsed float64, weeklyReset time.Time, fiveUsed float64, fiveReset time.Time) []byte {
	return []byte(fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":%g,"resets_at":%d},"seven_day":{"used_percentage":%g,"resets_at":%d}}}`,
		fiveUsed, fiveReset.Unix(), weeklyUsed, weeklyReset.Unix()))
}

func TestParseUsageWeekly(t *testing.T) {
	now := at("2026-11-03 14:00")
	w, err := parseUsage(usageJSON(61, at("2026-11-05 15:00")), now).weekly()
	if err != nil || w.Used != 61 || !w.Reset.Equal(at("2026-11-05 15:00")) || w.Rolled {
		t.Fatalf("got %+v %v", w, err)
	}
	if w.Remain() != 39 {
		t.Errorf("remain = %v", w.Remain())
	}

	// A file written before the last reset says nothing about the new week.
	w, err = parseUsage(usageJSON(95, at("2026-10-22 15:00")), now).weekly()
	if err != nil || w.Used != 0 || !w.Rolled || !w.Reset.Equal(at("2026-11-05 15:00")) {
		t.Fatalf("stale file: got %+v %v", w, err)
	}

	for _, body := range []string{`{}`, `{"rate_limits":{}}`, `{"rate_limits":{"seven_day":{"used_percentage":1}}}`, `nope`} {
		if _, err := parseUsage([]byte(body), now).weekly(); err == nil {
			t.Errorf("%s: expected an error", body)
		}
	}
}

func TestParseUsageFiveHour(t *testing.T) {
	now := at("2026-11-04 06:00")
	u := parseUsage(usageBoth(50, at("2026-11-05 15:00"), 42, at("2026-11-04 09:10")), now)
	fh, err := u.fiveHour()
	if err != nil || !fh.Active || fh.Used != 42 || fh.Remain() != 58 ||
		!fh.Reset.Equal(at("2026-11-04 09:10")) || !fh.Start().Equal(at("2026-11-04 04:10")) {
		t.Fatalf("running block: got %+v %v", fh, err)
	}

	// A block that has reset is over: no block runs, nothing is used.
	fh, err = parseUsage(usageBoth(50, at("2026-11-05 15:00"), 97, at("2026-11-04 05:30")), now).fiveHour()
	if err != nil || fh.Active || fh.Used != 0 {
		t.Fatalf("ended block: got %+v %v", fh, err)
	}

	// Each limit fails on its own.
	u = parseUsage([]byte(`{"rate_limits":{"seven_day":{"used_percentage":1,"resets_at":1793862000}}}`), now)
	if _, err := u.weekly(); err != nil {
		t.Errorf("weekly should still parse: %v", err)
	}
	if _, err := u.fiveHour(); err == nil {
		t.Error("missing five_hour: expected an error")
	}
	u = parseUsage([]byte(`{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":1793702400}}}`), now)
	if _, err := u.fiveHour(); err != nil {
		t.Errorf("five_hour should still parse: %v", err)
	}
	if _, err := u.weekly(); err == nil {
		t.Error("missing seven_day: expected an error")
	}
}

// --- decide -----------------------------------------------------------------

// weeklyOf reports a weekly figure and no running five-hour block.
func weeklyOf(used float64, reset string) func() usage {
	return func() usage { return usage{Weekly: weekly{Used: used, Reset: at(reset)}} }
}

// usageOf reports a weekly figure and a running five-hour block.
func usageOf(weeklyUsed float64, weeklyReset string, fiveUsed float64, fiveReset string) func() usage {
	return func() usage {
		return usage{
			Weekly:   weekly{Used: weeklyUsed, Reset: at(weeklyReset)},
			FiveHour: fiveHour{Used: fiveUsed, Reset: at(fiveReset), Active: true},
		}
	}
}

func noWeekly(t *testing.T) func() usage {
	return func() usage {
		t.Helper()
		t.Error("usage should not be needed")
		return usage{}
	}
}

func is(v bool) func() bool { return func() bool { return v } }

func TestDecideInsideWindow(t *testing.T) {
	cfg := testConfig()
	v := decide(cfg, at("2026-11-03 09:00"), is(true), weeklyOf(100, "2026-11-05 15:00"), false)
	if v.Hold || v.Alert != "" {
		t.Errorf("patron held or warned inside its window: %+v", v)
	}
	v = decide(cfg, at("2026-11-03 09:00"), is(false), noWeekly(t), false)
	if !v.Hold || !v.Until.Equal(at("2026-11-03 13:30")) {
		t.Errorf("other session should wait until 13:30: %+v", v)
	}
	// Near the five-hour limit the patron still runs, but is warned.
	v = decide(cfg, at("2026-11-03 09:00"), is(true), usageOf(10, "2026-11-05 15:00", 96, "2026-11-03 12:00"), false)
	if v.Hold || !strings.Contains(v.Alert, "96.0% used") || !strings.Contains(v.Alert, "Slow down") {
		t.Errorf("patron at 96%% inside its window: want a warning, got %+v", v)
	}
}

func TestDecideOutsideWindow(t *testing.T) {
	cfg := testConfig()
	cases := []struct {
		name   string
		now    string
		used   float64
		reset  string
		patron bool
		hold   bool
		until  string
	}{
		// 3 windows before the reset: 30% must stay.
		{"enough left", "2026-11-03 07:00", 69.9, "2026-11-05 15:00", false, false, ""},
		{"exactly the reserve", "2026-11-03 07:00", 70, "2026-11-05 15:00", false, true, "2026-11-03 13:30"},
		{"patron waits only until open", "2026-11-03 07:00", 70, "2026-11-05 15:00", true, true, "2026-11-03 08:30"},
		// After today's close, 2 windows are left: 20% must stay.
		{"after close, enough", "2026-11-03 14:00", 79, "2026-11-05 15:00", false, false, ""},
		{"after close, short", "2026-11-03 14:00", 85, "2026-11-05 15:00", false, true, "2026-11-04 13:30"},
		{"after close, patron", "2026-11-03 14:00", 85, "2026-11-05 15:00", true, true, "2026-11-04 08:30"},
		// No window left before the reset: nothing to keep.
		{"last day after close", "2026-11-05 14:00", 99, "2026-11-05 15:00", false, false, ""},
	}
	for _, c := range cases {
		v := decide(cfg, at(c.now), is(c.patron), weeklyOf(c.used, c.reset), false)
		if v.Hold != c.hold {
			t.Errorf("%s: hold = %v, want %v (%+v)", c.name, v.Hold, c.hold, v)
			continue
		}
		if c.hold && !v.Until.Equal(at(c.until)) {
			t.Errorf("%s: until %v, want %s", c.name, v.Until, c.until)
		}
	}
}

func TestDecideZeroReserve(t *testing.T) {
	cfg := testConfig()
	cfg.DailyReserve = 0
	if v := decide(cfg, at("2026-11-03 14:00"), is(false), weeklyOf(100, "2026-11-05 15:00"), false); v.Hold {
		t.Errorf("a zero reserve holds nothing: %+v", v)
	}
}

func TestDecideUnreadableUsage(t *testing.T) {
	v := decide(testConfig(), at("2026-11-03 14:00"), is(false), func() usage { return usage{Err: errors.New("boom")} }, false)
	if v.Hold || !strings.Contains(v.Unchecked, "boom") {
		t.Errorf("unreadable usage should allow, noting the skipped check: %+v", v)
	}
}

func TestDecideLooksUpPatronOnlyWhenNeeded(t *testing.T) {
	asked := false
	isPatron := func() bool { asked = true; return false }
	decide(testConfig(), at("2026-11-03 14:00"), isPatron, weeklyOf(10, "2026-11-05 15:00"), false)
	decide(testConfig(), at("2026-11-03 05:00"), isPatron, usageOf(10, "2026-11-05 15:00", 94, "2026-11-03 07:00"), false)
	if asked {
		t.Error("the session name was looked up although every session may run")
	}
}

func TestDecideFiveHourGuard(t *testing.T) {
	// Window 8:30-13:30, so the guard runs from 3:30 to 13:30. Threshold 95%.
	cfg := testConfig()
	custom := testConfig()
	custom.FiveHourThreshold = 80
	for _, c := range []struct {
		name   string
		cfg    config
		now    string
		used   float64
		reset  string
		patron bool
		hold   bool
		until  string
		alert  bool
	}{
		{"before the guard", cfg, "2026-11-03 03:29", 99, "2026-11-03 07:00", false, false, "", false},
		{"before the guard, patron", cfg, "2026-11-03 03:29", 99, "2026-11-03 07:00", true, false, "", false},
		{"guard starts 5h before the open", cfg, "2026-11-03 03:30", 95, "2026-11-03 07:00", false, true, "2026-11-03 13:30", false},
		{"below the threshold", cfg, "2026-11-03 04:00", 94.9, "2026-11-03 08:00", false, false, "", false},
		{"at the threshold", cfg, "2026-11-03 04:00", 95, "2026-11-03 08:00", false, true, "2026-11-03 13:30", false},
		{"patron is warned, not held", cfg, "2026-11-03 04:00", 95, "2026-11-03 08:00", true, false, "", true},
		{"patron below the threshold", cfg, "2026-11-03 04:00", 94.9, "2026-11-03 08:00", true, false, "", false},
		{"after the close", cfg, "2026-11-03 14:00", 99, "2026-11-03 18:00", false, false, "", false},
		{"after the close, patron", cfg, "2026-11-03 14:00", 99, "2026-11-03 18:00", true, false, "", false},
		{"Saturday", cfg, "2026-11-07 10:00", 99, "2026-11-07 12:00", false, false, "", false},
		{"Monday before the open", cfg, "2026-11-09 04:00", 99, "2026-11-09 06:00", false, true, "2026-11-09 13:30", false},
		{"own threshold", custom, "2026-11-03 04:00", 85, "2026-11-03 08:00", false, true, "2026-11-03 13:30", false},
	} {
		v := decide(c.cfg, at(c.now), is(c.patron), usageOf(10, "2026-11-05 15:00", c.used, c.reset), false)
		if v.Hold != c.hold || (v.Alert != "") != c.alert {
			t.Errorf("%s: hold %v alert %q, want hold %v alert %v", c.name, v.Hold, v.Alert, c.hold, c.alert)
			continue
		}
		if c.hold && (!v.Until.Equal(at(c.until)) || !v.FiveHourHold || !strings.Contains(v.Reason, "5-hour limit")) {
			t.Errorf("%s: want a 5-hour hold until %s, got %+v", c.name, c.until, v)
		}
	}

	// No block running: nothing is used, nothing is held.
	if v := decide(cfg, at("2026-11-03 04:00"), is(false), weeklyOf(10, "2026-11-05 15:00"), false); v.Hold {
		t.Errorf("no block running: should run, got %+v", v)
	}
	// Both limits short: the reason names both.
	v := decide(cfg, at("2026-11-03 04:00"), is(false), usageOf(75, "2026-11-05 15:00", 96, "2026-11-03 08:00"), false)
	if !v.Hold || !strings.Contains(v.Reason, "weekly reserve") || !strings.Contains(v.Reason, "5-hour limit") {
		t.Errorf("both short: got %+v", v)
	}
}

func TestDecideFiveHourTripped(t *testing.T) {
	// A call already held by the guard stays held until the close, even after
	// the block has reset and nothing is used.
	cfg := testConfig()
	v := decide(cfg, at("2026-11-03 07:30"), is(false), weeklyOf(10, "2026-11-05 15:00"), true)
	if !v.Hold || !v.FiveHourHold || !v.Until.Equal(at("2026-11-03 13:30")) {
		t.Errorf("tripped before the open: want a hold until 13:30, got %+v", v)
	}
	if v := decide(cfg, at("2026-11-03 07:30"), is(true), weeklyOf(10, "2026-11-05 15:00"), true); v.Hold {
		t.Errorf("the patron is never held by the guard: %+v", v)
	}
	if v := decide(cfg, at("2026-11-03 13:30"), is(false), weeklyOf(10, "2026-11-05 15:00"), true); v.Hold {
		t.Errorf("tripped, at the close: should run, got %+v", v)
	}
	unreadable := func() usage { return usage{Err: errors.New("share down")} }
	if v := decide(cfg, at("2026-11-03 07:30"), is(false), unreadable, true); v.Hold {
		t.Errorf("tripped, usage unreadable: fail open, got %+v", v)
	}
}

func TestDecideOneLimitMissing(t *testing.T) {
	cfg := testConfig()
	now := at("2026-11-03 04:00")
	v := decide(cfg, now, is(false), func() usage {
		return usage{Weekly: weekly{Used: 75, Reset: at("2026-11-05 15:00")}, FiveHourErr: errors.New("no five_hour")}
	}, false)
	if !v.Hold || !strings.Contains(v.Unchecked, "5-hour check skipped") {
		t.Errorf("weekly still enforced without five_hour: %+v", v)
	}
	v = decide(cfg, now, is(false), func() usage {
		return usage{WeeklyErr: errors.New("no seven_day"), FiveHour: fiveHour{Used: 96, Reset: at("2026-11-03 08:00"), Active: true}}
	}, false)
	if !v.Hold || !strings.Contains(v.Unchecked, "weekly reserve skipped") {
		t.Errorf("5-hour guard still enforced without seven_day: %+v", v)
	}
}

// --- session name -----------------------------------------------------------

type fakeFS map[string]string

func (f fakeFS) readFile(path string) ([]byte, error) {
	if body, ok := f[filepath.Clean(path)]; ok {
		return []byte(body), nil
	}
	return nil, fs.ErrNotExist
}

func (f fakeFS) glob(pattern string) ([]string, error) {
	var out []string
	for p := range f {
		if ok, _ := filepath.Match(filepath.Clean(pattern), p); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func fsSystem(files fakeFS, env map[string]string) *system {
	return &system{
		readFile:    files.readFile,
		glob:        files.glob,
		getenv:      func(k string) string { return env[k] },
		userHomeDir: func() (string, error) { return filepath.Clean("/home/u"), nil },
	}
}

func TestSessionNameFromRegistry(t *testing.T) {
	cfgDir := filepath.Clean("/home/u/.claude")
	transcript := filepath.Join(cfgDir, "projects", "D--proj", "sid-1.jsonl")
	files := fakeFS{
		filepath.Join(cfgDir, "sessions", "100.json"): `{"pid":100,"sessionId":"sid-1","name":"trader","updatedAt":5}`,
		filepath.Join(cfgDir, "sessions", "200.json"): `{"pid":200,"sessionId":"sid-2","name":"other","updatedAt":9}`,
		transcript: `{"type":"custom-title","customTitle":"old-title","sessionId":"sid-1"}`,
	}
	in := hookInput{SessionID: "sid-1", TranscriptPath: transcript}

	if got := sessionName(in, fsSystem(files, map[string]string{"CLAUDE_PID": "100"})); got != "trader" {
		t.Errorf("by CLAUDE_PID: %q", got)
	}
	// Without (or with a wrong) CLAUDE_PID the registry is scanned by session ID.
	if got := sessionName(in, fsSystem(files, map[string]string{"CLAUDE_PID": "200"})); got != "trader" {
		t.Errorf("by scan: %q", got)
	}
	// Not in the registry: the transcript's custom title is used.
	delete(files, filepath.Join(cfgDir, "sessions", "100.json"))
	if got := sessionName(in, fsSystem(files, nil)); got != "old-title" {
		t.Errorf("by transcript: %q", got)
	}
	if got := sessionName(hookInput{SessionID: "nobody"}, fsSystem(files, nil)); got != "" {
		t.Errorf("unknown session: %q", got)
	}
}

func TestLastCustomTitle(t *testing.T) {
	data := strings.Join([]string{
		`{"type":"custom-title","customTitle":"first","sessionId":"s"}`,
		`{"type":"user","message":{"content":"mentions \"custom-title\" in text"}}`,
		`{"type":"custom-title","customTitle":"second","sessionId":"s"}`,
	}, "\n")
	if got := lastCustomTitle([]byte(data)); got != "second" {
		t.Errorf("got %q", got)
	}
}

func TestPatronMatcher(t *testing.T) {
	m := &patronMatcher{in: hookInput{SessionID: "sid-1"}, name: "Trader", resolved: true}
	if !m.Matches("trader") {
		t.Error("names compare case-insensitively")
	}
	if !m.Matches("sid-1") {
		t.Error("a session ID matches too")
	}
	if m.Matches("trade") {
		t.Error("no partial match")
	}
	if fixedPatronMatcher("").Matches("trader") {
		t.Error("an unknown session is not the patron")
	}
}

// --- hook loop --------------------------------------------------------------

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }
func (c *fakeClock) set(s string)          { c.t = at(s) }
func (c *fakeClock) after(s string) bool   { return !c.t.Before(at(s)) }
func noSleep(t *testing.T) func(time.Duration) {
	return func(time.Duration) { t.Helper(); t.Error("the call should not have been held") }
}

type hookRun struct {
	files  fakeFS
	clock  *fakeClock
	stdout bytes.Buffer
	stderr bytes.Buffer
	sys    *system
	// onSleep runs after every sleep, to change the world mid-hold.
	onSleep func()
	sleeps  int
}

const (
	cfgPath   = "/share/bouncer.json"
	usagePath = "/share/usage.json"
)

func newHookRun(now string, policy string, usage []byte) *hookRun {
	h := &hookRun{
		files: fakeFS{
			filepath.Clean(cfgPath):                             policy,
			filepath.Clean(usagePath):                           string(usage),
			filepath.Clean("/home/u/.claude/sessions/100.json"): `{"sessionId":"sid-other","name":"research"}`,
			filepath.Clean("/home/u/.claude/sessions/200.json"): `{"sessionId":"sid-patron","name":"trader"}`,
		},
		clock: &fakeClock{t: at(now)},
	}
	h.sys = &system{
		stdout:      &h.stdout,
		stderr:      &h.stderr,
		now:         h.clock.now,
		readFile:    h.files.readFile,
		glob:        h.files.glob,
		getenv:      func(string) string { return "" },
		userHomeDir: func() (string, error) { return filepath.Clean("/home/u"), nil },
		alive:       func(int) bool { return true },
	}
	h.sys.sleep = func(d time.Duration) {
		h.sleeps++
		if h.sleeps > 100000 {
			panic("runaway hold loop")
		}
		h.clock.sleep(d)
		if h.onSleep != nil {
			h.onSleep()
		}
	}
	return h
}

func (h *hookRun) run(session string) int {
	in := fmt.Sprintf(`{"session_id":%q,"transcript_path":%q,"hook_event_name":"PreToolUse","tool_name":"Bash"}`,
		session, filepath.Clean("/home/u/.claude/projects/p/"+session+".jsonl"))
	h.sys.stdin = strings.NewReader(in)
	return run([]string{"-config", cfgPath, "-usage", usagePath}, h.sys)
}

const policy = `{"patron":"trader","start":"8:30","end":"13:30","daily_reserve":10}`

func TestHookRunsAtOnce(t *testing.T) {
	h := newHookRun("2026-11-03 09:00", policy, usageJSON(10, at("2026-11-05 15:00")))
	h.sys.sleep = noSleep(t)
	if code := h.run("sid-patron"); code != 0 || h.stdout.Len() != 0 || h.stderr.Len() != 0 {
		t.Fatalf("code %d, stdout %q, stderr %q", code, h.stdout.String(), h.stderr.String())
	}
}

func TestHookHoldsUntilClose(t *testing.T) {
	h := newHookRun("2026-11-03 09:00", policy, usageJSON(10, at("2026-11-05 15:00")))
	if code := h.run("sid-other"); code != 0 {
		t.Fatalf("code %d, stderr %q", code, h.stderr.String())
	}
	if !h.clock.t.Equal(at("2026-11-03 13:30")) {
		t.Errorf("released at %v, want 13:30", h.clock.t)
	}
	var out struct {
		SystemMessage      string `json:"systemMessage"`
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(h.stdout.Bytes(), &out); err != nil {
		t.Fatalf("release output %q: %v", h.stdout.String(), err)
	}
	if out.HookSpecificOutput.HookEventName != "PreToolUse" ||
		!strings.Contains(out.SystemMessage, "9:00 to 13:30") ||
		!strings.Contains(out.HookSpecificOutput.AdditionalContext, "2026-11-03 09:00 to 2026-11-03 13:30") {
		t.Errorf("unexpected release output: %+v", out)
	}
}

func TestHookReservePersistsAcrossDays(t *testing.T) {
	// 14:00 with 15% left and 2 windows (20%) to keep: held until the next
	// close, then re-evaluated with only 1 window (10%) left to keep.
	h := newHookRun("2026-11-03 14:00", policy, usageJSON(85, at("2026-11-05 15:00")))
	h.onSleep = func() {
		// The patron spends 3% during tomorrow's window.
		if h.clock.after("2026-11-04 13:30") {
			h.files[filepath.Clean(usagePath)] = string(usageJSON(88, at("2026-11-05 15:00")))
		}
	}
	if code := h.run("sid-other"); code != 0 {
		t.Fatalf("code %d, stderr %q", code, h.stderr.String())
	}
	if !h.clock.t.Equal(at("2026-11-04 13:30")) {
		t.Errorf("released at %v, want 11/4 13:30 (12%% left > 10%% reserve)", h.clock.t)
	}
}

func TestHookKeepsHoldingWhileStillShort(t *testing.T) {
	// The patron used its full share on 11/4, so at that close 9% is left for
	// 1 window (10%): hold again until 11/5 13:30, after which nothing is kept.
	h := newHookRun("2026-11-03 14:00", policy, usageJSON(85, at("2026-11-05 15:00")))
	h.onSleep = func() {
		if h.clock.after("2026-11-04 13:30") {
			h.files[filepath.Clean(usagePath)] = string(usageJSON(91, at("2026-11-05 15:00")))
		}
	}
	h.run("sid-other")
	if !h.clock.t.Equal(at("2026-11-05 13:30")) {
		t.Errorf("released at %v, want 11/5 13:30", h.clock.t)
	}
}

func TestHookPatronWakesAtOpen(t *testing.T) {
	h := newHookRun("2026-11-03 14:00", policy, usageJSON(85, at("2026-11-05 15:00")))
	h.run("sid-patron")
	if !h.clock.t.Equal(at("2026-11-04 08:30")) {
		t.Errorf("patron released at %v, want 11/4 8:30", h.clock.t)
	}
	if !strings.Contains(h.stdout.String(), "held this call from 11/3 14:00 to 11/4 8:30") {
		t.Errorf("a hold across days names both dates: %s", h.stdout.String())
	}
}

// hookOutput decodes the PreToolUse JSON the hook printed.
func hookOutput(t *testing.T, h *hookRun) (user, claude string) {
	t.Helper()
	var out struct {
		SystemMessage      string `json:"systemMessage"`
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(h.stdout.Bytes(), &out); err != nil || out.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("hook output %q: %v", h.stdout.String(), err)
	}
	return out.SystemMessage, out.HookSpecificOutput.AdditionalContext
}

func TestHookFiveHourGuardHoldsUntilClose(t *testing.T) {
	// 4:00, the block 2:00-7:00 is 96% used: others are held. At 7:00 the
	// block resets and a fresh one starts, but the hold lasts until the close.
	h := newHookRun("2026-11-03 04:00", policy, usageBoth(10, at("2026-11-05 15:00"), 96, at("2026-11-03 07:00")))
	h.onSleep = func() {
		if h.clock.after("2026-11-03 07:30") {
			h.files[filepath.Clean(usagePath)] = string(usageBoth(10, at("2026-11-05 15:00"), 3, at("2026-11-03 12:30")))
		}
	}
	h.run("sid-other")
	if !h.clock.t.Equal(at("2026-11-03 13:30")) {
		t.Errorf("released at %v, want the close at 13:30", h.clock.t)
	}
	if user, _ := hookOutput(t, h); !strings.Contains(user, "5-hour limit 96.0% used") {
		t.Errorf("release message should name the 5-hour limit: %s", user)
	}
}

func TestHookFiveHourWarnsPatron(t *testing.T) {
	usage := usageBoth(10, at("2026-11-05 15:00"), 96, at("2026-11-03 10:00"))
	for _, now := range []string{"2026-11-03 06:00", "2026-11-03 09:00"} { // before the open, and inside the window
		h := newHookRun(now, policy, usage)
		h.sys.sleep = noSleep(t)
		if code := h.run("sid-patron"); code != 0 {
			t.Fatalf("%s: code %d", now, code)
		}
		user, claude := hookOutput(t, h)
		if !strings.Contains(claude, "96.0% used") || !strings.Contains(claude, "Slow down") || !strings.Contains(user, "warned this session") {
			t.Errorf("%s: unexpected warning: %q / %q", now, user, claude)
		}
	}

	// Outside the guard the patron is left alone.
	h := newHookRun("2026-11-03 14:00", policy, usageBoth(10, at("2026-11-05 15:00"), 99, at("2026-11-03 18:00")))
	h.sys.sleep = noSleep(t)
	if code := h.run("sid-patron"); code != 0 || h.stdout.Len() != 0 {
		t.Errorf("after the close: code %d, stdout %q", code, h.stdout.String())
	}
}

func TestHookReleaseCarriesWarning(t *testing.T) {
	// The patron is held for the weekly reserve until the open; by then the
	// five-hour limit is high, so the release message warns it as well.
	h := newHookRun("2026-11-03 07:00", policy, usageBoth(70, at("2026-11-05 15:00"), 50, at("2026-11-03 11:00")))
	h.onSleep = func() {
		if h.clock.after("2026-11-03 08:00") {
			h.files[filepath.Clean(usagePath)] = string(usageBoth(70, at("2026-11-05 15:00"), 97, at("2026-11-03 11:00")))
		}
	}
	h.run("sid-patron")
	if !h.clock.t.Equal(at("2026-11-03 08:30")) {
		t.Fatalf("released at %v, want the open at 8:30", h.clock.t)
	}
	user, claude := hookOutput(t, h)
	if !strings.Contains(claude, "held this tool call") || !strings.Contains(claude, "Warning: the 5-hour limit is 97.0% used") ||
		!strings.Contains(user, "warned this session") {
		t.Errorf("unexpected release output: %q / %q", user, claude)
	}
}

func TestHookFridayCloseHoldsUntilMonday(t *testing.T) {
	h := newHookRun("2026-11-06 14:00", policy, usageJSON(75, at("2026-11-11 15:00")))
	h.run("sid-other")
	if !h.clock.t.Equal(at("2026-11-09 13:30")) {
		t.Errorf("released at %v, want Monday 11/9 13:30", h.clock.t)
	}
}

func TestHookPolicyEditReleases(t *testing.T) {
	h := newHookRun("2026-11-03 09:00", policy, usageJSON(10, at("2026-11-05 15:00")))
	h.onSleep = func() {
		if h.clock.after("2026-11-03 10:00") {
			h.files[filepath.Clean(cfgPath)] = `{"patron":"research","start":"8:30","end":"13:30","daily_reserve":10}`
		}
	}
	h.run("sid-other")
	if !h.clock.t.Equal(at("2026-11-03 10:00")) {
		t.Errorf("released at %v, want 10:00 (first poll after the edit)", h.clock.t)
	}
}

func TestHookRenamedPolicyReleases(t *testing.T) {
	// Renaming the policy file (or losing the share) switches the bouncer off,
	// which also releases calls that are already held.
	h := newHookRun("2026-11-03 09:00", policy, usageJSON(10, at("2026-11-05 15:00")))
	h.onSleep = func() {
		if h.clock.after("2026-11-03 10:00") {
			delete(h.files, filepath.Clean(cfgPath))
		}
	}
	if code := h.run("sid-other"); code != 0 || h.stderr.Len() != 0 {
		t.Fatalf("code %d, stderr %q", code, h.stderr.String())
	}
	if !h.clock.t.Equal(at("2026-11-03 10:00")) {
		t.Errorf("released at %v, want 10:00 (first poll after the rename)", h.clock.t)
	}
}

func TestHookLostUsageReleasesReserveHold(t *testing.T) {
	h := newHookRun("2026-11-03 14:00", policy, usageJSON(85, at("2026-11-05 15:00")))
	h.onSleep = func() {
		if h.clock.after("2026-11-03 15:00") {
			delete(h.files, filepath.Clean(usagePath))
		}
	}
	if code := h.run("sid-other"); code != 0 || h.stderr.Len() != 0 {
		t.Fatalf("code %d, stderr %q", code, h.stderr.String())
	}
	if !h.clock.t.Equal(at("2026-11-03 15:00")) {
		t.Errorf("released at %v, want 15:00 (first poll without a usage file)", h.clock.t)
	}
}

func TestHookStopsWhenClaudeExits(t *testing.T) {
	h := newHookRun("2026-11-03 09:00", policy, usageJSON(10, at("2026-11-05 15:00")))
	h.sys.getenv = func(k string) string {
		if k == "CLAUDE_PID" {
			return "4242"
		}
		return ""
	}
	h.sys.alive = func(pid int) bool { return pid == 4242 && h.clock.t.Before(at("2026-11-03 09:05")) }
	if code := h.run("sid-other"); code != 0 || h.stdout.Len() != 0 {
		t.Fatalf("code %d, stdout %q", code, h.stdout.String())
	}
	if !h.clock.t.Equal(at("2026-11-03 09:05")) {
		t.Errorf("stopped at %v, want 9:05", h.clock.t)
	}
}

func TestHookProblemsRunQuietly(t *testing.T) {
	quiet := func(name string, h *hookRun, code int) {
		t.Helper()
		if code != 0 || h.stdout.Len() != 0 || h.stderr.Len() != 0 {
			t.Errorf("%s: want a silent exit 0, got code %d, stdout %q, stderr %q", name, code, h.stdout.String(), h.stderr.String())
		}
	}

	h := newHookRun("2026-11-03 09:00", policy, usageJSON(10, at("2026-11-05 15:00")))
	h.sys.sleep = noSleep(t)
	delete(h.files, filepath.Clean(cfgPath))
	quiet("missing policy", h, h.run("sid-other"))

	h = newHookRun("2026-11-03 09:00", "{not json", usageJSON(10, at("2026-11-05 15:00")))
	h.sys.sleep = noSleep(t)
	quiet("broken policy", h, h.run("sid-other"))

	h = newHookRun("2026-11-03 14:00", policy, nil)
	h.sys.sleep = noSleep(t)
	delete(h.files, filepath.Clean(usagePath))
	quiet("missing usage file", h, h.run("sid-other"))

	h = newHookRun("2026-11-03 09:00", policy, usageJSON(10, at("2026-11-05 15:00")))
	h.sys.sleep = noSleep(t)
	h.sys.stdin = strings.NewReader("not json")
	quiet("bad stdin", h, hook(options{configPath: cfgPath, usagePath: usagePath, poll: time.Minute}, h.sys))
}

// --- command line -----------------------------------------------------------

func TestParseOptions(t *testing.T) {
	ok := [][]string{
		{"-config", "a", "-usage", "b"},
		{"--config=a", "--usage=b", "-poll", "30s"},
		{"-config", "a", "-usage", "b", "-dry-run", "-session", "x", "-now", "2026-11-03 09:00"},
		{"-version"},
	}
	for _, args := range ok {
		if _, err := parseOptions(args, &bytes.Buffer{}); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	bad := [][]string{
		{},
		{"-config", "a"},
		{"-usage", "b"},
		{"-config", "a", "-usage", "b", "extra"},
		{"-config", "a", "-usage", "b", "-nope"},
		{"-config", "a", "-usage", "b", "-poll", "0s"},
		{"-config", "a", "-usage", "b", "-session", "x"},
		{"-config", "a", "-usage", "b", "-now", "2026-11-03 09:00"},
	}
	for _, args := range bad {
		if _, err := parseOptions(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}

func TestRunNeverExitsTwo(t *testing.T) {
	sys := &system{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	for _, args := range [][]string{{"-nope"}, {"extra"}, {"-poll", "x"}} {
		if code := run(args, sys); code == 2 {
			t.Errorf("%v exited 2, which would block the tool call", args)
		}
	}
}

func TestDryRun(t *testing.T) {
	h := newHookRun("2026-11-03 14:00", policy, usageJSON(85, at("2026-11-05 15:00")))
	h.sys.sleep = noSleep(t)
	h.sys.stdinIsTerminal = true
	code := run([]string{"-config", cfgPath, "-usage", usagePath, "-dry-run", "-session", "research"}, h.sys)
	out := h.stdout.String()
	if code != 0 || !strings.Contains(out, "reserve   10.0% x 2 window(s) before reset = 20.0%") ||
		!strings.Contains(out, "decision  hold until 11/4 13:30") {
		t.Errorf("code %d, output:\n%s", code, out)
	}
}

func TestDryRunFiveHour(t *testing.T) {
	usage := usageBoth(10, at("2026-11-05 15:00"), 96, at("2026-11-03 07:00"))
	for session, wants := range map[string][]string{
		"research": {
			"5-hour    used 96.0%, block 2:00-7:00, threshold 95.0% (default)",
			"guard     on since 3:30, until the close 13:30",
			"decision  hold until 13:30: 5-hour limit 96.0% used (threshold 95.0%)",
		},
		"trader": {
			"decision  run, and warn the patron: the 5-hour limit is 96.0% used",
		},
	} {
		h := newHookRun("2026-11-03 04:00", policy, usage)
		h.sys.sleep = noSleep(t)
		h.sys.stdinIsTerminal = true
		code := run([]string{"-config", cfgPath, "-usage", usagePath, "-dry-run", "-session", session}, h.sys)
		out := h.stdout.String()
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s: missing %q in output:\n%s", session, want, out)
			}
		}
		if code != 0 {
			t.Errorf("%s: code %d", session, code)
		}
	}

	h := newHookRun("2026-11-03 02:00", policy, usage)
	h.sys.stdinIsTerminal = true
	run([]string{"-config", cfgPath, "-usage", usagePath, "-dry-run", "-session", "research"}, h.sys)
	if out := h.stdout.String(); !strings.Contains(out, "guard     off until 3:30, 5h before the open") {
		t.Errorf("before the guard:\n%s", out)
	}
}

func TestDryRunWithoutPolicy(t *testing.T) {
	h := newHookRun("2026-11-03 09:00", policy, usageJSON(10, at("2026-11-05 15:00")))
	h.sys.stdinIsTerminal = true
	delete(h.files, filepath.Clean(cfgPath))
	code := run([]string{"-config", cfgPath, "-usage", usagePath, "-dry-run", "-session", "research"}, h.sys)
	if out := h.stdout.String(); code != 0 || !strings.Contains(out, "bouncer is off") {
		t.Errorf("code %d, output:\n%s", code, out)
	}
}

func TestReadFileRetryNotFound(t *testing.T) {
	_, err := readFileRetry(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("got %v", err)
	}
}
