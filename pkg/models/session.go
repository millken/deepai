package models

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SessionState represents the lifecycle state of a conversation session.
type SessionState string

const (
	SessionStateActive    SessionState = "active"
	SessionStateCompleted SessionState = "completed"
	SessionStateArchived  SessionState = "archived"
)

// Validate reports whether the session state is supported.
func (s SessionState) Validate() error {
	switch s {
	case SessionStateActive, SessionStateCompleted, SessionStateArchived:
		return nil
	default:
		return fmt.Errorf("invalid session state %q", s)
	}
}

// Session holds the durable state for a single conversation thread.
type Session struct {
	ID        string            `json:"id"`
	UserID    string            `json:"user_id"`
	State     SessionState      `json:"state"`
	Messages  []Message         `json:"messages,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at,omitempty"`
	UpdatedAt time.Time         `json:"updated_at,omitempty"`
}

// NewSession constructs a new active session with initialized timestamps.
func NewSession(id, userID string) Session {
	now := time.Now().UTC()
	return Session{
		ID:        strings.TrimSpace(id),
		UserID:    strings.TrimSpace(userID),
		State:     SessionStateActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Validate checks whether the session and all nested messages are valid.
func (s Session) Validate() error {
	if strings.TrimSpace(s.ID) == "" {
		return errors.New("session id is required")
	}
	if strings.TrimSpace(s.UserID) == "" {
		return errors.New("session user_id is required")
	}
	if err := s.State.Validate(); err != nil {
		return err
	}
	if !s.UpdatedAt.IsZero() && s.UpdatedAt.Before(s.CreatedAt) {
		return errors.New("session updated_at cannot be before created_at")
	}
	for i, msg := range s.Messages {
		if msg.SessionID != s.ID {
			return fmt.Errorf("messages[%d]: session_id %q does not match session id %q", i, msg.SessionID, s.ID)
		}
		if err := msg.Validate(); err != nil {
			return fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	return nil
}

// AddMessage appends a validated message to the session and updates timestamps.
func (s *Session) AddMessage(msg Message) error {
	if s == nil {
		return errors.New("session is nil")
	}
	if strings.TrimSpace(msg.SessionID) == "" {
		msg.SessionID = s.ID
	}
	if msg.SessionID != s.ID {
		return fmt.Errorf("message session_id %q does not match session id %q", msg.SessionID, s.ID)
	}
	if err := msg.Validate(); err != nil {
		return err
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	s.Messages = append(s.Messages, msg)
	if s.CreatedAt.IsZero() {
		s.CreatedAt = msg.CreatedAt
	}
	s.UpdatedAt = msg.CreatedAt
	return nil
}

// LastMessage returns the latest message in the session, if any.
func (s Session) LastMessage() (Message, bool) {
	if len(s.Messages) == 0 {
		return Message{}, false
	}
	return s.Messages[len(s.Messages)-1], true
}

// SetState updates the session state and refreshes the update timestamp.
func (s *Session) SetState(state SessionState) error {
	if s == nil {
		return errors.New("session is nil")
	}
	if err := state.Validate(); err != nil {
		return err
	}
	s.State = state
	s.Touch()
	return nil
}

// Touch refreshes the session update timestamp.
func (s *Session) Touch() {
	if s == nil {
		return
	}
	s.UpdatedAt = time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = s.UpdatedAt
	}
}
