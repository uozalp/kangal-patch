/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maintenance

import (
	"fmt"
	"strings"
	"time"

	"github.com/uozalp/kangal-patch/api/v1alpha1"
)

// IsInMaintenanceWindow checks if the current time is within an allowed maintenance window
func IsInMaintenanceWindow(spec *v1alpha1.MaintenanceSpec, now time.Time) (bool, string) {
	// If no maintenance spec is provided, always allow patching
	if spec == nil {
		return true, ""
	}

	// Check if current date is excluded
	if isExcludedDate(spec.ExcludeDates, now) {
		return false, fmt.Sprintf("current date %s is in excluded dates", now.Format("2006-01-02"))
	}

	// If no windows are defined, allow patching
	if len(spec.Windows) == 0 {
		return true, ""
	}

	// Check if current time is within any enabled window
	for _, window := range spec.Windows {
		if window.Disabled {
			continue
		}

		inWindow, err := isInWindow(window, now)
		if err != nil {
			// Log error but continue checking other windows
			continue
		}
		if inWindow {
			return true, ""
		}
	}

	return false, "current time is not within any maintenance window"
}

// isExcludedDate checks if the given date is in the excluded dates list
func isExcludedDate(excludeDates []string, now time.Time) bool {
	currentDate := now.Format("2006-01-02")
	for _, excludeDate := range excludeDates {
		// Parse date in YYYY-MM-DD format only
		excludeDateParsed, err := time.Parse("2006-01-02", excludeDate)
		if err != nil {
			// Skip invalid date format
			continue
		}

		if excludeDateParsed.Format("2006-01-02") == currentDate {
			return true
		}
	}
	return false
}

// isInWindow checks if the given time is within the maintenance window
func isInWindow(window v1alpha1.MaintenanceWindow, now time.Time) (bool, error) {
	// Use UTC for all time comparisons
	nowInTZ := now.UTC()

	// Check if current day matches
	if !isDayMatch(window.Days, nowInTZ) {
		return false, nil
	}

	// Parse start and end times
	startTime, err := parseTimeOfDay(window.StartTime, nowInTZ)
	if err != nil {
		return false, fmt.Errorf("invalid start time: %w", err)
	}

	endTime, err := parseTimeOfDay(window.EndTime, nowInTZ)
	if err != nil {
		return false, fmt.Errorf("invalid end time: %w", err)
	}

	// Check if current time is within the window
	// Handle case where window spans midnight
	if endTime.Before(startTime) {
		// Window spans midnight (e.g., 22:00 - 02:00)
		return nowInTZ.After(startTime) || nowInTZ.Before(endTime) || nowInTZ.Equal(startTime), nil
	}

	// Normal case
	return (nowInTZ.After(startTime) || nowInTZ.Equal(startTime)) && nowInTZ.Before(endTime), nil
}

// isDayMatch checks if the current day matches the window's day specification
func isDayMatch(days []string, now time.Time) bool {
	// If no days specified, match all days
	if len(days) == 0 {
		return true
	}

	// Check for special "Any" value
	for _, day := range days {
		if strings.EqualFold(day, "Any") || strings.EqualFold(day, "All") {
			return true
		}
	}

	// Get current weekday
	currentDay := now.Weekday().String()
	currentDayShort := currentDay[:3] // e.g., "Mon", "Tue"

	// Check if current day is in the list
	for _, day := range days {
		// Match full name (case-insensitive)
		if strings.EqualFold(day, currentDay) {
			return true
		}
		// Match 3-letter abbreviation (case-insensitive)
		if strings.EqualFold(day, currentDayShort) {
			return true
		}
	}

	return false
}

// parseTimeOfDay parses a time string in HH:MM format and returns a time.Time
// with the date set to the given reference date
func parseTimeOfDay(timeStr string, referenceDate time.Time) (time.Time, error) {
	parts := strings.Split(timeStr, ":")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("invalid time format: %s", timeStr)
	}

	var hour, minute int
	_, err := fmt.Sscanf(timeStr, "%d:%d", &hour, &minute)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse time: %w", err)
	}

	return time.Date(
		referenceDate.Year(),
		referenceDate.Month(),
		referenceDate.Day(),
		hour,
		minute,
		0, 0,
		referenceDate.Location(),
	), nil
}

// NextMaintenanceWindow returns the next time a maintenance window will open
// Useful for determining when to retry if outside maintenance window
func NextMaintenanceWindow(spec *v1alpha1.MaintenanceSpec, now time.Time) (time.Time, error) {
	if spec == nil || len(spec.Windows) == 0 {
		return now, nil // No windows means always available
	}

	var nextWindow time.Time
	found := false

	// Check each window for the next 7 days
	for i := range 7 {
		checkDate := now.AddDate(0, 0, i)

		// Skip excluded dates
		if isExcludedDate(spec.ExcludeDates, checkDate) {
			continue
		}

		for _, window := range spec.Windows {
			if window.Disabled {
				continue
			}

			checkDateInTZ := checkDate.UTC()

			if !isDayMatch(window.Days, checkDateInTZ) {
				continue
			}

			startTime, err := parseTimeOfDay(window.StartTime, checkDateInTZ)
			if err != nil {
				continue
			}

			// If this start time is in the future (or now) and earlier than our current best
			if (startTime.After(now) || startTime.Equal(now)) && (!found || startTime.Before(nextWindow)) {
				nextWindow = startTime
				found = true
			}
		}
	}

	if !found {
		return time.Time{}, fmt.Errorf("no maintenance window found in the next 7 days")
	}

	return nextWindow, nil
}
