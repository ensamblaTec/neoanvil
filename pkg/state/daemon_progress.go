// Package state — daemon progress visibility: active task, queue summary, HUD payload. [132.C]
package state

import (
	"encoding/json"

	"go.etcd.io/bbolt"
)

// DaemonActiveTask holds the in-flight task details emitted in PullTasks responses. [132.C]
type DaemonActiveTask struct {
	Description string `json:"description"`
	TargetFile  string `json:"target_file"`
	StartedAt   int64  `json:"started_at"`
	Retries     int    `json:"retries"`
}

// DaemonQueueSummary is the snapshot of all task lifecycle counters plus budget. [132.C]
type DaemonQueueSummary struct {
	Pending         int `json:"pending"`
	InProgress      int `json:"in_progress"`
	Done            int `json:"done"`
	Failed          int `json:"failed"`
	BudgetRemaining int `json:"budget_remaining"`
}

// DaemonProgressPayload is the SSE event body for EventDaemonProgress. [132.C]
type DaemonProgressPayload struct {
	TasksDone  int                `json:"tasks_done"`
	TasksTotal int                `json:"tasks_total"`
	ActiveTask *DaemonActiveTask  `json:"active_task,omitempty"`
	Summary    DaemonQueueSummary `json:"summary"`
}

// GetDaemonActiveTask returns the first in_progress task, or nil if none exists.
func GetDaemonActiveTask() (*DaemonActiveTask, error) {
	if plannerDB == nil {
		return nil, nil
	}
	var active *DaemonActiveTask
	err := plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			if active != nil {
				return nil // already found one
			}
			var t SRETask
			if json.Unmarshal(v, &t) != nil {
				return nil
			}
			if t.LifecycleState == TaskLifecycleInProgress {
				active = &DaemonActiveTask{
					Description: t.Description,
					TargetFile:  t.TargetFile,
					StartedAt:   t.StartedAt,
					Retries:     t.Retries,
				}
			}
			return nil
		})
	})
	return active, err
}

// GetDaemonQueueSummary counts tasks per lifecycle state and computes budget_remaining. [132.C]
func GetDaemonQueueSummary(sessionID string, sessionBudget int) (DaemonQueueSummary, error) {
	var s DaemonQueueSummary
	if plannerDB == nil {
		return s, nil
	}
	err := plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var t SRETask
			if json.Unmarshal(v, &t) != nil {
				return nil
			}
			switch t.LifecycleState {
			case TaskLifecyclePending:
				s.Pending++
			case TaskLifecycleInProgress:
				s.InProgress++
			case TaskLifecycleCompleted:
				s.Done++
			case TaskLifecycleFailedPermanent:
				s.Failed++
			case TaskLifecycleOrphaned:
				s.Failed++ // orphaned counts as failed for summary purposes
			}
			return nil
		})
	})
	if err != nil {
		return s, err
	}
	if sessionID != "" {
		budget, berr := DaemonBudgetGet(sessionID)
		if berr == nil && sessionBudget > 0 {
			remaining := max(sessionBudget-budget.TokensUsed, 0)
			s.BudgetRemaining = remaining
		}
	}
	return s, nil
}

// BuildDaemonProgressPayload assembles the full HUD event payload. [132.C]
func BuildDaemonProgressPayload(sessionID string, sessionBudget int) (DaemonProgressPayload, error) {
	summary, err := GetDaemonQueueSummary(sessionID, sessionBudget)
	if err != nil {
		return DaemonProgressPayload{}, err
	}
	active, err := GetDaemonActiveTask()
	if err != nil {
		return DaemonProgressPayload{}, err
	}
	total := summary.Pending + summary.InProgress + summary.Done + summary.Failed
	return DaemonProgressPayload{
		TasksDone:  summary.Done,
		TasksTotal: total,
		ActiveTask: active,
		Summary:    summary,
	}, nil
}
