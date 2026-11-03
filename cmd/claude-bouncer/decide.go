package main

import (
	"fmt"
	"strings"
	"time"
)

// guardLead is how long before the window opens the five-hour guard starts:
// one block, since a block started in that time is still running at the open.
const guardLead = fiveHourPeriod

// verdict is the outcome of one evaluation, with the figures behind it.
type verdict struct {
	Hold   bool
	Until  time.Time // when a held call should be evaluated again
	Reason string    // human-readable explanation of Hold

	// Alert is a warning for the patron, delivered with a call that runs.
	Alert string

	// Unchecked names the checks that could not be made, and why. A check
	// that cannot be made is skipped, never turned into a hold.
	Unchecked string

	InWindow bool
	Window   window // current window when InWindow, otherwise the next one

	PatronChecked bool
	Patron        bool

	WeeklyChecked bool
	Weekly        weekly
	Windows       int     // exclusive windows left before the weekly reset
	Reserve       float64 // percent of the weekly limit still kept for the patron

	Guarded         bool // between guardLead before the window opens and its close
	FiveHourChecked bool
	FiveHour        fiveHour
	FiveHourHigh    bool // five-hour usage is at or above the threshold
	FiveHourHold    bool // the hold comes from the five-hour guard
}

// decide applies the rules of the policy.
//
//  1. Inside the exclusive window the patron runs freely; every other session
//     is held until the window ends.
//  2. Outside it, every session runs while the weekly limit left is above
//     daily_reserve x (windows left before the weekly reset). Otherwise every
//     session is held until the next window closes, except the patron, which
//     is held only until the next window opens, where rule 1 lets it run.
//  3. From five hours before the window opens until it closes, five-hour
//     usage at or above five_hour_threshold holds every other session until
//     the close, and warns the patron on every call without holding it.
//
// tripped reports that this call is already held by rule 3: such a hold
// lasts until the close even if the five-hour block resets meanwhile.
//
// isPatron and loadUsage are called only when the outcome depends on them.
func decide(cfg config, now time.Time, isPatron func() bool, loadUsage func() usage, tripped bool) verdict {
	var v verdict
	patron := func() bool {
		if !v.PatronChecked {
			v.PatronChecked = true
			v.Patron = isPatron()
		}
		return v.Patron
	}

	if w, ok := cfg.currentWindow(now); ok {
		v.InWindow = true
		v.Window = w
		v.Guarded = true
		if !patron() {
			v.Hold = true
			v.Until = w.End
			v.Reason = fmt.Sprintf("exclusive window for %q until %s", cfg.Patron, formatMinute(w.End, now))
			return v
		}
		u := loadUsage()
		if fh, err := u.fiveHour(); err != nil {
			v.Unchecked = fmt.Sprintf("5-hour check skipped: %v", err)
		} else {
			v.checkFiveHour(cfg, fh)
		}
		if v.FiveHourHigh {
			v.Alert = fiveHourAlert(v.FiveHour, now)
		}
		return v
	}

	v.Window = cfg.nextWindow(now)
	v.Guarded = !now.Before(v.Window.Start.Add(-guardLead))
	u := loadUsage()
	var unchecked []string

	weeklyShort := false
	if wk, err := u.weekly(); err != nil {
		unchecked = append(unchecked, fmt.Sprintf("weekly reserve skipped: %v", err))
	} else {
		v.WeeklyChecked = true
		v.Weekly = wk
		v.Windows = cfg.windowsBefore(now, wk.Reset)
		v.Reserve = round1(cfg.DailyReserve * float64(v.Windows))
		weeklyShort = v.Reserve > 0 && wk.Remain() <= v.Reserve
	}
	if v.Guarded {
		if fh, err := u.fiveHour(); err != nil {
			unchecked = append(unchecked, fmt.Sprintf("5-hour check skipped: %v", err))
		} else {
			v.checkFiveHour(cfg, fh)
		}
	}
	v.Unchecked = strings.Join(unchecked, "; ")

	fiveHourHold := v.FiveHourChecked && (v.FiveHourHigh || tripped) && !patron()
	if !weeklyShort && !fiveHourHold {
		if v.FiveHourHigh && patron() {
			v.Alert = fiveHourAlert(v.FiveHour, now)
		}
		return v
	}

	v.Hold = true
	var reasons []string
	if weeklyShort {
		reasons = append(reasons, fmt.Sprintf("weekly reserve for %q: %s%% left, %s%% x %d window(s) = %s%% kept until reset %s",
			cfg.Patron,
			formatPercent(v.Weekly.Remain()),
			formatPercent(cfg.DailyReserve),
			v.Windows,
			formatPercent(v.Reserve),
			formatMinute(v.Weekly.Reset, now)))
	}
	if fiveHourHold {
		v.FiveHourHold = true
		if v.FiveHourHigh {
			reasons = append(reasons, fmt.Sprintf("5-hour limit %s%% used (threshold %s%%) before the window for %q opens at %s",
				formatPercent(v.FiveHour.Used), formatPercent(cfg.FiveHourThreshold), cfg.Patron, formatMinute(v.Window.Start, now)))
		} else {
			reasons = append(reasons, fmt.Sprintf("5-hour limit reached its threshold before the window for %q opens; held until the close", cfg.Patron))
		}
	}
	v.Reason = strings.Join(reasons, "; ")
	if patron() {
		v.Until = v.Window.Start
	} else {
		v.Until = v.Window.End
	}
	return v
}

// checkFiveHour records the five-hour figures. A block that is not running
// has nothing used, so it never reaches the threshold.
func (v *verdict) checkFiveHour(cfg config, fh fiveHour) {
	v.FiveHourChecked = true
	v.FiveHour = fh
	v.FiveHourHigh = fh.Active && fh.Used >= cfg.FiveHourThreshold
}

// fiveHourAlert is the warning the patron gets with each call while the
// five-hour limit is close to running out.
func fiveHourAlert(fh fiveHour, now time.Time) string {
	return fmt.Sprintf("the 5-hour limit is %s%% used and its block resets at %s. Slow down your inference pace, or the 5-hour rate limit may stop you until then.",
		formatPercent(fh.Used), formatMinute(fh.Reset, now))
}

func formatPercent(v float64) string {
	return fmt.Sprintf("%.1f", round1(v))
}

// formatMinute renders a local time as H:MM, prefixed with M/D unless it falls
// on the same date as ref.
func formatMinute(t, ref time.Time) string {
	t = t.In(ref.Location())
	clockPart := fmt.Sprintf("%d:%02d", t.Hour(), t.Minute())
	ty, tm, td := t.Date()
	ry, rm, rd := ref.Date()
	if ty == ry && tm == rm && td == rd {
		return clockPart
	}
	return fmt.Sprintf("%d/%d %s", tm, td, clockPart)
}
