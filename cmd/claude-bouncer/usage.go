package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	// weeklyPeriod is the length of Claude's seven-day rate-limit window.
	weeklyPeriod = 7 * 24 * time.Hour
	// fiveHourPeriod is the length of one five-hour block. A block starts with
	// the first request after the previous one ended and resets at resets_at.
	fiveHourPeriod = 5 * time.Hour
)

// usage is what the usage file says about both limits. Each limit has its own
// error, so a file that lacks one limit only switches off that limit's check.
type usage struct {
	Err         error // the file itself could not be read or parsed
	Weekly      weekly
	WeeklyErr   error
	FiveHour    fiveHour
	FiveHourErr error
}

func (u usage) weekly() (weekly, error) {
	if u.Err != nil {
		return weekly{}, u.Err
	}
	return u.Weekly, u.WeeklyErr
}

func (u usage) fiveHour() (fiveHour, error) {
	if u.Err != nil {
		return fiveHour{}, u.Err
	}
	return u.FiveHour, u.FiveHourErr
}

// weekly is the seven-day limit as seen at a given moment.
type weekly struct {
	Used   float64   // percent of the weekly limit already used
	Reset  time.Time // when the current weekly window resets
	Rolled bool      // the file's window had already reset; Used is assumed 0
}

func (w weekly) Remain() float64 {
	return round1(100 - w.Used)
}

// fiveHour is the five-hour limit as seen at a given moment.
type fiveHour struct {
	Used   float64   // percent of the running block already used; 0 when none runs
	Reset  time.Time // when the running block resets, or when the last one did
	Active bool      // a block is running now
}

func (f fiveHour) Remain() float64 {
	return round1(100 - f.Used)
}

// Start is when the running block began.
func (f fiveHour) Start() time.Time {
	return f.Reset.Add(-fiveHourPeriod)
}

// usageFile is the file published by `continue-claude -export`: Claude Code's
// own rate_limits object from the status-line JSON.
type usageFile struct {
	RateLimits *struct {
		FiveHour *rateLimit `json:"five_hour"`
		SevenDay *rateLimit `json:"seven_day"`
	} `json:"rate_limits"`
}

type rateLimit struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       *int64   `json:"resets_at"`
}

func (l *rateLimit) values(name string) (used float64, reset time.Time, err error) {
	if l == nil {
		return 0, time.Time{}, fmt.Errorf("usage file has no rate_limits.%s", name)
	}
	if l.UsedPercentage == nil || l.ResetsAt == nil {
		return 0, time.Time{}, fmt.Errorf("usage file's %s lacks used_percentage or resets_at", name)
	}
	return *l.UsedPercentage, time.Unix(*l.ResetsAt, 0), nil
}

// parseUsage extracts both limits as they stand at now.
//
// A seven-day window that has already reset is stale: the usage of the new
// window is unknown but started from zero, so it is taken as 0 and the reset
// is rolled forward. A five-hour block that has already reset is simply over:
// no block runs until the next request starts one, so nothing is used.
func parseUsage(data []byte, now time.Time) usage {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var f usageFile
	if err := json.Unmarshal(data, &f); err != nil {
		return usage{Err: fmt.Errorf("parse usage file: %w", err)}
	}
	if f.RateLimits == nil {
		return usage{Err: errors.New("usage file has no rate_limits")}
	}

	var u usage
	if used, reset, err := f.RateLimits.SevenDay.values("seven_day"); err != nil {
		u.WeeklyErr = err
	} else {
		u.Weekly = weekly{Used: used, Reset: reset.In(now.Location())}
		if !u.Weekly.Reset.After(now) {
			periods := now.Sub(u.Weekly.Reset)/weeklyPeriod + 1
			u.Weekly.Reset = u.Weekly.Reset.Add(periods * weeklyPeriod)
			u.Weekly.Used = 0
			u.Weekly.Rolled = true
		}
	}
	if used, reset, err := f.RateLimits.FiveHour.values("five_hour"); err != nil {
		u.FiveHourErr = err
	} else {
		u.FiveHour = fiveHour{Reset: reset.In(now.Location())}
		if u.FiveHour.Reset.After(now) {
			u.FiveHour.Used = used
			u.FiveHour.Active = true
		}
	}
	return u
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
