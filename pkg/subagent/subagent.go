package subagent

import "time"

func (t *Task) CreatedAt() string {
	if t == nil || t.createdAt.IsZero() {
		return ""
	}
	return t.createdAt.Format(time.RFC3339Nano)
}

func (t *Task) CompletedAt() string {
	if t == nil || t.completedAt.IsZero() {
		return ""
	}
	return t.completedAt.Format(time.RFC3339Nano)
}
