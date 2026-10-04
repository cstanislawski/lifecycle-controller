package controller

import (
	"math"
	"time"

	"github.com/robfig/cron/v3"
)

// recurringSchedule describes the two decisions needed by recurring restart
// reconciliation: the next occurrence and the anchor to persist after missed
// occurrences are coalesced.
type recurringSchedule interface {
	Next(time.Time) time.Time
	coalescedAnchor(lastRestart, now time.Time) time.Time
}

type intervalRecurringSchedule struct {
	duration time.Duration
}

func (s intervalRecurringSchedule) Next(anchor time.Time) time.Time {
	return anchor.Add(s.duration)
}

func (s intervalRecurringSchedule) coalescedAnchor(lastRestart, now time.Time) time.Time {
	return now.Add(-elapsedModulo(lastRestart, now, s.duration))
}

func elapsedModulo(start, end time.Time, interval time.Duration) time.Duration {
	// time.Time.Sub saturates at the largest duration, so retain only each
	// chunk's modulo while advancing across anchors farther than ~292 years.
	var remainder time.Duration
	for cursor := start; cursor.Before(end); {
		chunk := end.Sub(cursor)
		next := end
		if chunk == time.Duration(math.MaxInt64) {
			next = cursor.Add(chunk)
		}

		chunk %= interval
		if remainder >= interval-chunk {
			remainder -= interval - chunk
		} else {
			remainder += chunk
		}
		cursor = next
	}
	return remainder
}

type cronRecurringSchedule struct {
	cron.Schedule
}

func (cronRecurringSchedule) coalescedAnchor(_ time.Time, now time.Time) time.Time {
	return now
}

func planRecurringRestart(schedule recurringSchedule, lastRestart, now time.Time) (scheduledAt, coalescedAnchor time.Time, due bool) {
	scheduledAt = schedule.Next(lastRestart)
	if now.Before(scheduledAt) {
		return scheduledAt, lastRestart, false
	}
	return scheduledAt, schedule.coalescedAnchor(lastRestart, now), true
}
