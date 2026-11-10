package bot

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/millken/deepai/pkg/schedule"
)

// REST + WebSocket adapters for timed tasks. All responses carry the
// scheduler mode ("active"/"standby") so a client talking to a standby
// instance knows another process is executing the task.
// docs/BOT_SCHEDULER_DESIGN.md §9.

func (s *Server) scheduleOr404(w http.ResponseWriter) (*schedule.Service, bool) {
	if s.schedules == nil {
		writeError(w, http.StatusNotFound, "not_found", "schedules are disabled")
		return nil, false
	}
	return s.schedules, true
}

func (s *Server) scheduleResult(data map[string]any) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	if s.schedules != nil {
		data["scheduler"] = s.schedules.Mode()
	}
	return data
}

func scheduleBody(w http.ResponseWriter, r *http.Request, allowEmpty bool) (map[string]any, bool) {
	var m map[string]any
	if !readJSON(w, r, &m, allowEmpty) {
		return nil, false
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, true
}

func paramString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func paramInt(m map[string]any, key string, def int) int {
	if m == nil {
		return def
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil {
			return n
		}
	}
	return def
}

func paramBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	return false
}

// ─────────────────────────── REST ───────────────────────────

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := svc.List(r.Context(), r.URL.Query().Get("state"), limit)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleResult(map[string]any{"schedules": list}))
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	body, ok := scheduleBody(w, r, false)
	if !ok {
		return
	}
	in, err := schedule.CreateInputFromMap(body, svc.Config(), r.URL.Query().Get("session_id"), "")
	if err != nil {
		s.writeErr(w, err)
		return
	}
	sch, err := svc.Create(r.Context(), in)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.scheduleResult(map[string]any{"schedule": sch}))
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	sch, err := svc.Get(r.Context(), id)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	runs, _ := svc.Runs(r.Context(), id, 5)
	writeJSON(w, http.StatusOK, s.scheduleResult(map[string]any{"schedule": sch, "runs": runs}))
}

func (s *Server) handlePatchSchedule(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	body, ok := scheduleBody(w, r, false)
	if !ok {
		return
	}
	patch, err := schedule.PatchFromMap(body, svc.Config())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	sch, err := svc.Update(r.Context(), r.PathValue("id"), patch)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleResult(map[string]any{"schedule": sch}))
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	cancel := false
	if v := r.URL.Query().Get("cancel"); v != "" {
		cancel, _ = strconv.ParseBool(v)
	}
	_, err := svc.Delete(r.Context(), r.PathValue("id"), cancel)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePauseSchedule(w http.ResponseWriter, r *http.Request) {
	s.writeScheduleTransition(w, r, "pause")
}

func (s *Server) handleResumeSchedule(w http.ResponseWriter, r *http.Request) {
	s.writeScheduleTransition(w, r, "resume")
}

func (s *Server) handleApproveSchedule(w http.ResponseWriter, r *http.Request) {
	s.writeScheduleTransition(w, r, "approve")
}

func (s *Server) writeScheduleTransition(w http.ResponseWriter, r *http.Request, kind string) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var (
		sch schedule.Schedule
		err error
	)
	switch kind {
	case "pause":
		sch, err = svc.Pause(r.Context(), id)
	case "resume":
		sch, err = svc.Resume(r.Context(), id)
	default:
		sch, err = svc.Approve(r.Context(), id)
	}
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleResult(map[string]any{"schedule": sch}))
}

func (s *Server) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	body, ok := scheduleBody(w, r, true)
	if !ok {
		return
	}
	run, err := svc.RunNow(r.Context(), r.PathValue("id"), paramString(body, "request_id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.scheduleResult(map[string]any{"run": run}))
}

func (s *Server) handleSkipSchedule(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	run, err := svc.Skip(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleResult(map[string]any{"run": run}))
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := svc.Runs(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleResult(map[string]any{"runs": runs}))
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	run, err := svc.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleResult(map[string]any{"run": run}))
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	run, err := svc.CancelRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.scheduleResult(map[string]any{"run": run}))
}

func (s *Server) handleValidateSchedule(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.scheduleOr404(w)
	if !ok {
		return
	}
	body, ok := scheduleBody(w, r, false)
	if !ok {
		return
	}
	tr, err := schedule.TriggerFromMap(body["trigger"])
	if err != nil {
		s.writeErr(w, err)
		return
	}
	v, err := svc.Validate(r.Context(), tr, paramString(body, "timezone"), paramInt(body, "limit", 3))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleResult(map[string]any{
		"trigger": v.Trigger, "timezone": v.Timezone,
		"summary": v.Summary, "next_runs": v.NextRuns,
	}))
}

// ─────────────────────────── WebSocket ───────────────────────────

// handleScheduleWS mirrors the REST ops. The bool reports whether the op was
// a schedule op, so wsConn.handle can fall through to its "unknown op" error.
func (s *Server) handleScheduleWS(ctx context.Context, req WSRequest) (any, bool, error) {
	if !isScheduleOp(req.Op) {
		return nil, false, nil
	}
	if s.schedules == nil {
		return nil, true, schedule.NewError(schedule.CodeNotFound, "schedules are disabled")
	}
	svc := s.schedules
	params := req.Params
	if params == nil {
		params = map[string]any{}
	}
	id := req.ScheduleID
	if id == "" {
		id = paramString(params, "id")
	}
	runID := req.RunID
	if runID == "" {
		runID = paramString(params, "run_id")
	}
	switch req.Op {
	case "list_schedules":
		list, err := svc.List(ctx, paramString(params, "state"), paramInt(params, "limit", req.Limit))
		return s.scheduleResult(map[string]any{"schedules": list}), true, err
	case "get_schedule":
		sch, err := svc.Get(ctx, id)
		return s.scheduleResult(map[string]any{"schedule": sch}), true, err
	case "create_schedule":
		in, err := schedule.CreateInputFromMap(params, svc.Config(), paramString(params, "session_id"), "")
		if err != nil {
			return nil, true, err
		}
		sch, err := svc.Create(ctx, in)
		return s.scheduleResult(map[string]any{"schedule": sch}), true, err
	case "update_schedule":
		patch, err := schedule.PatchFromMap(params, svc.Config())
		if err != nil {
			return nil, true, err
		}
		sch, err := svc.Update(ctx, id, patch)
		return s.scheduleResult(map[string]any{"schedule": sch}), true, err
	case "delete_schedule", "cancel_schedule":
		_, err := svc.Delete(ctx, id, paramBool(params, "cancel"))
		return s.scheduleResult(nil), true, err
	case "pause_schedule":
		sch, err := svc.Pause(ctx, id)
		return s.scheduleResult(map[string]any{"schedule": sch}), true, err
	case "resume_schedule":
		sch, err := svc.Resume(ctx, id)
		return s.scheduleResult(map[string]any{"schedule": sch}), true, err
	case "approve_schedule":
		sch, err := svc.Approve(ctx, id)
		return s.scheduleResult(map[string]any{"schedule": sch}), true, err
	case "run_schedule":
		run, err := svc.RunNow(ctx, id, paramString(params, "request_id"))
		return s.scheduleResult(map[string]any{"run": run}), true, err
	case "skip_schedule":
		run, err := svc.Skip(ctx, id)
		return s.scheduleResult(map[string]any{"run": run}), true, err
	case "list_runs":
		runs, err := svc.Runs(ctx, id, paramInt(params, "limit", req.Limit))
		return s.scheduleResult(map[string]any{"runs": runs}), true, err
	case "get_run":
		run, err := svc.GetRun(ctx, runID)
		return s.scheduleResult(map[string]any{"run": run}), true, err
	case "cancel_run":
		run, err := svc.CancelRun(ctx, runID)
		return s.scheduleResult(map[string]any{"run": run}), true, err
	case "validate_trigger":
		tr, err := schedule.TriggerFromMap(params["trigger"])
		if err != nil {
			return nil, true, err
		}
		v, err := svc.Validate(ctx, tr, paramString(params, "timezone"), paramInt(params, "limit", 3))
		return s.scheduleResult(map[string]any{
			"trigger": v.Trigger, "timezone": v.Timezone,
			"summary": v.Summary, "next_runs": v.NextRuns,
		}), true, err
	}
	return nil, false, nil
}

func isScheduleOp(op string) bool {
	switch op {
	case "list_schedules", "get_schedule", "create_schedule", "update_schedule",
		"delete_schedule", "cancel_schedule", "pause_schedule", "resume_schedule",
		"approve_schedule", "run_schedule", "skip_schedule", "list_runs",
		"get_run", "cancel_run", "validate_trigger":
		return true
	}
	return false
}
