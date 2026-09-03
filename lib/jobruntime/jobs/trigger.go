package jobs

import "time"

type TriggerType string

const (
	TriggerScheduled TriggerType = "SCHEDULED"
	TriggerManual    TriggerType = "MANUAL"
	TriggerRetry     TriggerType = "RETRY"
)

type Trigger struct {
	Type   TriggerType
	Entity string
	Meta   map[string]any
}

type JobMeta struct {
	JobID   string
	JobKind string
	RunID   string
	SlotTS  time.Time
	Trigger Trigger
}
