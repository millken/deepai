package agent

import (
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// nestedEventSink adapts run_code's journal events into AgentEvents with
// ParentID set (RUN_CODE_DESIGN §7.2 Phase 2): "running" maps to
// AgentEventToolCallStart, every other status to AgentEventToolCallEnd.
// interrupted has no models.CallStatus equivalent — it arrives as a failed
// end event whose Error explains the interruption, mirroring how the outer
// run_code result classifies it.
//
// The sink fires from batch worker goroutines (and once from the Run
// goroutine's abort snapshot); a.emit is non-blocking and lock-guarded, so
// this is safe.
func (a *Agent) nestedEventSink(sessionID, parentCallID string) tools.NestedEventSink {
	return func(e tools.NestedToolEvent) {
		id := parentCallID + "." + itoa(e.Seq)
		status := models.CallStatusCompleted
		evtType := AgentEventToolCallEnd
		errText := ""
		switch e.Status {
		case "running":
			evtType = AgentEventToolCallStart
			status = models.CallStatusRunning
		case "failed":
			status = models.CallStatusFailed
			errText = e.Err
		case "interrupted":
			status = models.CallStatusFailed
			errText = e.Err
			if errText == "" {
				errText = "interrupted"
			}
		}
		te := &ToolCallEvent{
			ID:         id,
			Name:       e.Tool,
			ParentID:   parentCallID,
			Status:     status,
			DurationMS: e.MS,
			Error:      errText,
		}
		a.emit(AgentEvent{
			Type:      evtType,
			SessionID: sessionID,
			ToolEvent: te,
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
