package tracker

import "time"

type Status string

const (
	StatusIdle       Status = "idle"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

type Entry struct {
	Session      string    `json:"session"`
	Pane         string    `json:"pane"`
	Status       Status    `json:"status"`
	Description  string    `json:"description"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at"`
	Acknowledged bool      `json:"acknowledged"`
}
