package bot

import (
	"context"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// Runtime is what the Manager needs from the agent side. The production
// implementation (AgentRuntime) wires pkg/agent with the same tools, skills,
// memory and models as the interactive REPL; tests plug in a fake.
type Runtime interface {
	// Info describes the runtime for GET /v1/info.
	Info() Info
	// DefaultModel is the model alias a session starts with.
	DefaultModel() string
	// HasModel reports whether alias names a configured model.
	HasModel(alias string) bool
	// NewConversation returns the per-session agent state (sandbox, carried
	// skill/breaker state). The Manager calls it once per loaded session and
	// closes it when the session is evicted or deleted.
	NewConversation(sessionID string) (Conversation, error)
}

// Conversation runs the turns of one session. The Manager never runs two
// turns of the same Conversation concurrently.
type Conversation interface {
	RunTurn(ctx context.Context, in TurnInput) (*TurnOutput, error)
	Close() error
}

// TurnInput is one turn's request.
type TurnInput struct {
	SessionID string
	TurnID    string
	Model     string
	// Messages is the session transcript, ending with the new user message.
	Messages []models.Message
	// Emit publishes a turn event. Seq, SessionID, TurnID and Time are
	// filled in by the Manager.
	Emit func(Event)
	// Interaction answers ask_clarification by asking connected clients.
	Interaction tools.UserInteraction
}

// TurnOutput is what a finished (or interrupted) turn produced.
type TurnOutput struct {
	// Messages is the full transcript after the turn. It is returned even
	// when RunTurn also returns an error, so partial progress is persisted.
	Messages    []models.Message
	FinalOutput string
	Usage       *agent.Usage
	ToolCalls   int
	LLMTurns    int
}

// Store persists sessions. models.SessionRepository (the SQLite store the
// CLI uses) satisfies it, so bot sessions show up in `deepai session list`
// and can be resumed with `deepai -r <id>`, and vice versa.
type Store interface {
	Create(opts models.CreateOpts) (*models.Session, error)
	Load(id string) (*models.Session, error)
	LoadMessages(id string) ([]models.Message, error)
	AppendMessage(sessionID string, msg models.Message) error
	ListRecent(limit int) ([]models.SessionMeta, error)
	Rename(id, title string) error
	Delete(id string) error
}

// Locker is the optional single-writer lock a Store may offer; the SQLite
// store does. While a turn runs the Manager holds the session's lock so a
// CLI process resuming the same session cannot interleave writes.
type Locker interface {
	AcquireSessionLock(sessionID string, owner models.LockOwner, force bool) error
	RefreshSessionLock(sessionID string, owner models.LockOwner) error
	ReleaseSessionLock(sessionID string, owner models.LockOwner) error
}
