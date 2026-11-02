package main

import (
	"fmt"
	"time"
)

// verdict is the outcome of one evaluation, with the figures behind it.
type verdict struct {
	Hold   bool
	Until  time.Time // when a held call should be evaluated again
	Reason string    // human-readable explanation of Hold

	// Warning is set when the reserve could not be checked; the call is
	// allowed in that case.
	Warning string

	InWindow bool
	Window   window // current window when InWindow, otherwise the next one

	PatronChecked bool
	Patron        bool

	WeeklyChecked bool
	Weekly        weekly
	Windows       int     // exclusive windows left before the weekly reset
	Reserve       float64 // percent of the weekly limit still kept for the patron
}

// decide applies the two rules of the policy.
//
//  1. Inside the exclusive window the patron runs freely; every other session
//     is held until the window ends.
//  2. Outside it, every session runs while the weekly limit left is above
//     daily_reserve x (windows left before the weekly reset). Otherwise every
//     session is held until the next window closes, except the patron, which
//     is held only until the next window opens, where rule 1 lets it run.
//
// isPatron and loadWeekly are called only when the outcome depends on them.
func decide(cfg config, now time.Time, isPatron func() bool, loadWeekly func() (weekly, error)) verdict {
	var v verdict

	if w, ok := cfg.currentWindow(now); ok {
		v.InWindow = true
		v.Window = w
		v.PatronChecked = true
		v.Patron = isPatron()
		if !v.Patron {
			v.Hold = true
			v.Until = w.End
			v.Reason = fmt.Sprintf("exclusive window for %q until %s", cfg.Patron, formatMinute(w.End, now))
		}
		return v
	}

	v.Window = cfg.nextWindow(now)
	wk, err := loadWeekly()
	if err != nil {
		v.Warning = fmt.Sprintf("weekly reserve not enforced: %v", err)
		return v
	}
	v.WeeklyChecked = true
	v.Weekly = wk
	v.Windows = cfg.windowsBefore(now, wk.Reset)
	v.Reserve = round1(cfg.DailyReserve * float64(v.Windows))

	if v.Reserve <= 0 || wk.Remain() > v.Reserve {
		return v
	}

	v.Hold = true
	v.PatronChecked = true
	v.Patron = isPatron()
	if v.Patron {
		v.Until = v.Window.Start
	} else {
		v.Until = v.Window.End
	}
	v.Reason = fmt.Sprintf("weekly reserve for %q: %s%% left, %s%% x %d window(s) = %s%% kept until reset %s",
		cfg.Patron,
		formatPercent(wk.Remain()),
		formatPercent(cfg.DailyReserve),
		v.Windows,
		formatPercent(v.Reserve),
		formatMinute(wk.Reset, now))
	return v
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
