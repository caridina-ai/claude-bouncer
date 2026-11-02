package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// weeklyPeriod is the length of Claude's seven-day rate-limit window.
const weeklyPeriod = 7 * 24 * time.Hour

// weekly is the seven-day limit as seen at a given moment.
type weekly struct {
	Used   float64   // percent of the weekly limit already used
	Reset  time.Time // when the current weekly window resets
	Rolled bool      // the file's window had already reset; Used is assumed 0
}

func (w weekly) Remain() float64 {
	return round1(100 - w.Used)
}

// usageFile is the file published by `continue-claude -export`: Claude Code's
// own rate_limits object from the status-line JSON.
type usageFile struct {
	RateLimits *struct {
		SevenDay *struct {
			UsedPercentage *float64 `json:"used_percentage"`
			ResetsAt       *int64   `json:"resets_at"`
		} `json:"seven_day"`
	} `json:"rate_limits"`
}

// parseWeekly extracts the seven-day limit. When the recorded window has
// already reset, the figure is stale: the usage of the new window is unknown
// but started from zero, so it is taken as 0 and the reset is rolled forward.
func parseWeekly(data []byte, now time.Time) (weekly, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var f usageFile
	if err := json.Unmarshal(data, &f); err != nil {
		return weekly{}, fmt.Errorf("parse usage file: %w", err)
	}
	if f.RateLimits == nil || f.RateLimits.SevenDay == nil {
		return weekly{}, errors.New("usage file has no rate_limits.seven_day")
	}
	sd := f.RateLimits.SevenDay
	if sd.UsedPercentage == nil || sd.ResetsAt == nil {
		return weekly{}, errors.New("usage file's seven_day lacks used_percentage or resets_at")
	}

	w := weekly{
		Used:  *sd.UsedPercentage,
		Reset: time.Unix(*sd.ResetsAt, 0).In(now.Location()),
	}
	if !w.Reset.After(now) {
		periods := now.Sub(w.Reset)/weeklyPeriod + 1
		w.Reset = w.Reset.Add(periods * weeklyPeriod)
		w.Used = 0
		w.Rolled = true
	}
	return w, nil
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
