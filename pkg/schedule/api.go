package schedule

import "time"

// CreateInputFromMap is the shared argument decoder for the REST and
// WebSocket transports. The tool uses the same validation path internally.
func CreateInputFromMap(m map[string]any, cfg Config, sessionID, turnID string) (CreateInput, error) {
	in, err := createInputFromArgs(m, cfg, sessionID, turnID)
	if err != nil {
		return CreateInput{}, err
	}
	in.CreatedBy = CreatedBy{Kind: "user", SessionID: sessionID, TurnID: turnID, Client: strArg(m, "client")}
	if state := strArg(m, "state"); state == string(StatePendingApproval) {
		in.State = StatePendingApproval
	}
	return in, nil
}

// PatchFromMap decodes a partial update from a REST/WS body.
func PatchFromMap(m map[string]any, cfg Config) (Patch, error) {
	p, _, _, err := patchFromArgs(m, cfg)
	return p, err
}

// TriggerFromMap decodes a trigger object (used by /validate).
func TriggerFromMap(v any) (Trigger, error) { return triggerFromArg(v) }

// TimeFromMap parses an optional time string in the task's timezone.
func TimeFromMap(v any, tz string) (*time.Time, error) { return timeArg(v, tz) }
