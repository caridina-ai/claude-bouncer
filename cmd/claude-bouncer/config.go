package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// config is the shared policy file. It has exactly four fields, all required.
type config struct {
	Patron       string  // session name (or session ID) that owns the exclusive window
	Start        clock   // weekday start of the exclusive window, local time
	End          clock   // weekday end of the exclusive window, local time
	DailyReserve float64 // percent of the weekly limit kept for the patron per window
}

type configFile struct {
	Patron       *string  `json:"patron"`
	Start        *string  `json:"start"`
	End          *string  `json:"end"`
	DailyReserve *float64 `json:"daily_reserve"`
}

// clock is a time of day, minute precision.
type clock struct {
	hour   int
	minute int
}

var clockPattern = regexp.MustCompile(`^([0-9]{1,2}):([0-9]{2})$`)

func parseClock(value string) (clock, error) {
	m := clockPattern.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return clock{}, fmt.Errorf("%q is not a HH:MM time", value)
	}
	h, _ := strconv.Atoi(m[1])
	mm, _ := strconv.Atoi(m[2])
	if h > 23 || mm > 59 {
		return clock{}, fmt.Errorf("%q is not a valid time of day", value)
	}
	return clock{hour: h, minute: mm}, nil
}

func (c clock) String() string {
	return fmt.Sprintf("%d:%02d", c.hour, c.minute)
}

// on returns this time of day on the calendar date of day, in day's location.
func (c clock) on(day time.Time) time.Time {
	y, m, d := day.Date()
	return time.Date(y, m, d, c.hour, c.minute, 0, 0, day.Location())
}

func parseConfig(data []byte) (config, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // tolerate a UTF-8 BOM from Windows editors

	var raw configFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return config{}, fmt.Errorf("parse config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return config{}, errors.New("parse config: unexpected data after the JSON object")
	}

	var missing []string
	if raw.Patron == nil {
		missing = append(missing, "patron")
	}
	if raw.Start == nil {
		missing = append(missing, "start")
	}
	if raw.End == nil {
		missing = append(missing, "end")
	}
	if raw.DailyReserve == nil {
		missing = append(missing, "daily_reserve")
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("config is missing %s", strings.Join(missing, ", "))
	}

	cfg := config{
		Patron:       strings.TrimSpace(*raw.Patron),
		DailyReserve: *raw.DailyReserve,
	}
	if cfg.Patron == "" {
		return config{}, errors.New("config: patron must not be empty")
	}
	var err error
	if cfg.Start, err = parseClock(*raw.Start); err != nil {
		return config{}, fmt.Errorf("config: start: %w", err)
	}
	if cfg.End, err = parseClock(*raw.End); err != nil {
		return config{}, fmt.Errorf("config: end: %w", err)
	}
	if cfg.Start == cfg.End {
		return config{}, errors.New("config: start and end must differ")
	}
	if cfg.DailyReserve < 0 || cfg.DailyReserve > 100 {
		return config{}, errors.New("config: daily_reserve must be between 0 and 100")
	}
	return cfg, nil
}

// window is one exclusive period [Start, End).
type window struct {
	Start time.Time
	End   time.Time
}

// windowStarting returns the exclusive window that starts on the calendar date
// of day. A window whose end is not after its start runs past midnight.
func (cfg config) windowStarting(day time.Time) window {
	start := cfg.Start.on(day)
	end := cfg.End.on(day)
	if !end.After(start) {
		y, m, d := day.Date()
		end = cfg.End.on(time.Date(y, m, d+1, 0, 0, 0, 0, day.Location()))
	}
	return window{Start: start, End: end}
}

// isTradingDay reports whether an exclusive window starts on this date. The
// patron only needs its window Monday to Friday.
func isTradingDay(day time.Time) bool {
	wd := day.Weekday()
	return wd != time.Saturday && wd != time.Sunday
}

// windowsFrom calls yield for each window that has not ended by now, in order,
// until yield returns false. Windows start on weekdays only. The scan is
// bounded so bad data cannot loop forever.
func (cfg config) windowsFrom(now time.Time, yield func(window) bool) {
	y, m, d := now.Date()
	for i := -1; i <= 400; i++ {
		day := time.Date(y, m, d+i, 12, 0, 0, 0, now.Location())
		if !isTradingDay(day) {
			continue
		}
		w := cfg.windowStarting(day)
		if !w.End.After(now) {
			continue
		}
		if !yield(w) {
			return
		}
	}
}

// currentWindow reports the window that contains now, if any.
func (cfg config) currentWindow(now time.Time) (window, bool) {
	var found window
	ok := false
	cfg.windowsFrom(now, func(w window) bool {
		if !now.Before(w.Start) {
			found, ok = w, true
		}
		return false
	})
	return found, ok
}

// nextWindow returns the first window that has not ended by now.
func (cfg config) nextWindow(now time.Time) window {
	var found window
	cfg.windowsFrom(now, func(w window) bool {
		found = w
		return false
	})
	return found
}

// windowsBefore counts the windows that have not ended by now and start
// before reset. A window that straddles reset counts in full.
func (cfg config) windowsBefore(now, reset time.Time) int {
	n := 0
	cfg.windowsFrom(now, func(w window) bool {
		if !w.Start.Before(reset) {
			return false
		}
		n++
		return true
	})
	return n
}
