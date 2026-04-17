package memory

import (
	"net/url"
	"strings"
)

type ScopeType string

const (
	ScopeSession ScopeType = "session"
	ScopeUser    ScopeType = "user"
	ScopeGroup   ScopeType = "group"
	ScopeAgent   ScopeType = "agent"
)

const scopeKeyPrefix = "__scope__:"

type Scope struct {
	Type      ScopeType `json:"type"`
	ID        string    `json:"id"`
	Namespace string    `json:"namespace,omitempty"`
}

func SessionScope(id string) Scope {
	return Scope{Type: ScopeSession, ID: id}
}

func UserScope(id string) Scope {
	return Scope{Type: ScopeUser, ID: id}
}

func GroupScope(id string) Scope {
	return Scope{Type: ScopeGroup, ID: id}
}

func AgentScope(id string) Scope {
	return Scope{Type: ScopeAgent, ID: id}
}

func (s Scope) Normalized() Scope {
	scopeType := ScopeType(strings.TrimSpace(string(s.Type)))
	if scopeType == "" {
		scopeType = ScopeSession
	}
	return Scope{
		Type:      scopeType,
		ID:        strings.TrimSpace(s.ID),
		Namespace: strings.TrimSpace(s.Namespace),
	}
}

func (s Scope) Key() string {
	scope := s.Normalized()
	if scope.Type == ScopeSession && scope.Namespace == "" {
		return scope.ID
	}
	if scope.Type == ScopeAgent && scope.Namespace == "" {
		return "agent:" + scope.ID
	}
	return scopeKeyPrefix +
		url.PathEscape(string(scope.Type)) + ":" +
		url.PathEscape(scope.ID) + ":" +
		url.PathEscape(scope.Namespace)
}

func ParseScopeKey(key string) Scope {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "agent:") {
		return AgentScope(strings.TrimPrefix(key, "agent:"))
	}
	if !strings.HasPrefix(key, scopeKeyPrefix) {
		return SessionScope(key)
	}
	parts := strings.SplitN(strings.TrimPrefix(key, scopeKeyPrefix), ":", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return Scope{
		Type:      ScopeType(unescapeScopePart(parts[0])),
		ID:        unescapeScopePart(parts[1]),
		Namespace: unescapeScopePart(parts[2]),
	}.Normalized()
}

func unescapeScopePart(raw string) string {
	value, err := url.PathUnescape(raw)
	if err != nil {
		return raw
	}
	return value
}
