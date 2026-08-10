package domain

import "time"

// AuditEvent is the bounded, non-secret audit projection exposed to users.
// Store-private detail is deliberately excluded because historical detail may
// contain operational metadata that is not safe for a general timeline.
type AuditEvent struct {
	ID         string    `json:"id"`
	ActorID    string    `json:"actorId"`
	Action     string    `json:"action"`
	TargetType string    `json:"targetType"`
	TargetID   string    `json:"targetId"`
	Outcome    string    `json:"outcome"`
	RequestID  string    `json:"requestId,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

type AuditEventQuery struct {
	TargetType string
	TargetID   string
	Action     string
	Limit      int
}
