package main

import "sync"

// aiJobReservation keeps the UTC day and hashed counters of one accepted job.
// It must only be refunded if the queue abandons the job before starting it.
type aiJobReservation struct {
	mu           sync.Mutex
	quota        *aiQuota
	day          string
	subjectKey   string
	secondaryKey string
	refunded     bool
}

// reserveJob atomically reserves a trial unit and returns a day-bound refund
// handle. Unlike reading the day after reserve(), this cannot race midnight.
func (q *aiQuota) reserveJob(subject string, limit int, secondary string, secondaryLimit int) (bool, *aiJobReservation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.resetDayLocked()
	subjectKey := q.hashedKey(subject)
	if q.state.Total >= 40 || q.state.Visitors[subjectKey] >= limit {
		return false, nil, nil
	}
	secondaryKey := ""
	if secondary != "" {
		secondaryKey = q.hashedKey(secondary)
		if q.state.Visitors[secondaryKey] >= secondaryLimit {
			return false, nil, nil
		}
	}
	q.state.Total++
	q.state.Visitors[subjectKey]++
	if secondaryKey != "" {
		q.state.Visitors[secondaryKey]++
	}
	if err := q.saveLocked(); err != nil {
		q.state.Total--
		q.state.Visitors[subjectKey]--
		if secondaryKey != "" {
			q.state.Visitors[secondaryKey]--
		}
		return false, nil, err
	}
	return true, &aiJobReservation{quota: q, day: q.state.Day, subjectKey: subjectKey, secondaryKey: secondaryKey}, nil
}

// Refund is idempotent. A reservation from yesterday can never decrement
// today's counters, including when its queue timeout fires after midnight.
func (r *aiJobReservation) Refund() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refunded {
		return nil
	}
	q := r.quota
	q.mu.Lock()
	defer q.mu.Unlock()
	q.resetDayLocked()
	if q.state.Day != r.day || q.state.Total == 0 || q.state.Visitors[r.subjectKey] == 0 {
		r.refunded = true
		return nil
	}
	q.state.Total--
	q.state.Visitors[r.subjectKey]--
	secondaryDecremented := r.secondaryKey != "" && q.state.Visitors[r.secondaryKey] > 0
	if secondaryDecremented {
		q.state.Visitors[r.secondaryKey]--
	}
	if err := q.saveLocked(); err != nil {
		q.state.Total++
		q.state.Visitors[r.subjectKey]++
		if secondaryDecremented {
			q.state.Visitors[r.secondaryKey]++
		}
		return err
	}
	r.refunded = true
	return nil
}
