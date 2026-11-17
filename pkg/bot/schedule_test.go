package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/schedule"
	"golang.org/x/net/websocket"
)

func newScheduleTestService(t *testing.T, m *Manager) (*schedule.Service, *ScheduleDispatcher) {
	t.Helper()
	cfg := schedule.DefaultConfig()
	cfg.Timezone = "UTC"
	cfg.MinInterval = time.Second
	cfg.Confirm = "auto"
	svc, err := schedule.NewService(schedule.NewMemoryStore(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	disp := AttachSchedule(m, svc)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = disp.Wait(ctx)
	})
	return svc, disp
}

func waitRunStatus(t *testing.T, svc *schedule.Service, id string, want schedule.RunStatus) schedule.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := svc.GetRun(context.Background(), id)
		if err == nil && r.Status == want {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s", id, want)
	return schedule.Run{}
}

func TestScheduleDispatcherFireAndFinish(t *testing.T) {
	m, _, _ := newTestManager(t)
	svc, _ := newScheduleTestService(t, m)
	sub, _, _ := m.SubscribeAll(-1)
	defer sub.Close()

	sch, err := svc.Create(context.Background(), schedule.CreateInput{
		Name: "每日总结", Prompt: "汇总今天的工作",
		Trigger:  schedule.Trigger{Type: schedule.TriggerOnce, In: "1h"},
		Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.RunNow(context.Background(), sch.ID, "req-fire")
	if err != nil {
		t.Fatal(err)
	}
	finished := waitRunStatus(t, svc, run.ID, schedule.RunCompleted)
	if finished.SessionID == "" || finished.TurnID == "" || finished.Output == "" {
		t.Fatalf("finished run = %+v", finished)
	}
	// The dedicated session is pinned on the schedule by Fire's return value.
	got, err := svc.Get(context.Background(), sch.ID)
	if err != nil || got.SessionID == "" || got.RunCount != 1 {
		t.Fatalf("schedule after run = %+v, %v", got, err)
	}
	info, msgs, err := m.GetSession(finished.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if info.MsgCount < 2 || len(msgs) < 2 {
		t.Fatalf("scheduled turn transcript = %+v, %d messages", info, len(msgs))
	}
	if msgs[0].Metadata["source"] != "schedule" || msgs[0].Metadata["schedule_id"] != sch.ID {
		t.Fatalf("user message metadata = %v", msgs[0].Metadata)
	}
	if !strings.Contains(msgs[0].Content, "[定时任务]") || !strings.Contains(msgs[0].Content, sch.Prompt) {
		t.Fatalf("scheduled message = %q", msgs[0].Content)
	}

	seen := map[EventType]bool{}
	deadline := time.After(time.Second)
	for !seen[EventScheduleRunStarted] || !seen[EventScheduleRunFinished] {
		select {
		case e := <-sub.C:
			seen[e.Type] = true
		case <-deadline:
			t.Fatalf("global events = %v, want run_started + run_finished", seen)
		}
	}
}

func TestScheduleREST(t *testing.T) {
	m, _, _ := newTestManager(t)
	svc, _ := newScheduleTestService(t, m)
	srv := NewServer(ServerOptions{Manager: m, Schedules: svc, DisableWebUI: true})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	base := ts.URL

	resp := doJSON(t, "POST", base+"/v1/schedules", "", `{"name":"提醒","prompt":"检查 CI","trigger":{"type":"once","in":"1h"},"timezone":"UTC"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var created struct {
		Scheduler string            `json:"scheduler"`
		Schedule  schedule.Schedule `json:"schedule"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Scheduler != "active" || created.Schedule.ID == "" || created.Schedule.Summary == "" {
		t.Fatalf("create body = %+v", created)
	}
	id := created.Schedule.ID

	if r := doJSON(t, "GET", base+"/v1/schedules", "", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", r.StatusCode)
	}
	if r := doJSON(t, "GET", base+"/v1/schedules/"+id, "", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", r.StatusCode)
	}
	if r := doJSON(t, "PATCH", base+"/v1/schedules/"+id, "", `{"name":"改过的名字"}`, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d", r.StatusCode)
	}
	if r := doJSON(t, "POST", base+"/v1/schedule/validate", "", `{"trigger":{"type":"interval","every":"1h"},"timezone":"UTC"}`, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("validate status = %d", r.StatusCode)
	}
	if r := doJSON(t, "POST", base+"/v1/schedule/validate", "", `{"trigger":{"type":"cron","expr":"0 22 * * *"}}`, nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("cron validate status = %d", r.StatusCode)
	}
	if r := doJSON(t, "POST", base+"/v1/schedules/"+id+"/pause", "", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("pause status = %d", r.StatusCode)
	}
	if r := doJSON(t, "POST", base+"/v1/schedules/"+id+"/resume", "", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("resume status = %d", r.StatusCode)
	}
	if r := doJSON(t, "POST", base+"/v1/schedules/"+id+"/skip", "", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("skip status = %d", r.StatusCode)
	}
	runResp := doJSON(t, "POST", base+"/v1/schedules/"+id+"/run", "", `{"request_id":"rest-run"}`, nil)
	if runResp.StatusCode != http.StatusAccepted {
		t.Fatalf("run status = %d", runResp.StatusCode)
	}
	var runBody struct {
		Run schedule.Run `json:"run"`
	}
	if err := json.NewDecoder(runResp.Body).Decode(&runBody); err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, svc, runBody.Run.ID, schedule.RunCompleted)
	if r := doJSON(t, "GET", base+"/v1/schedules/"+id+"/runs", "", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("runs status = %d", r.StatusCode)
	}
	if r := doJSON(t, "GET", base+"/v1/runs/"+runBody.Run.ID, "", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("get run status = %d", r.StatusCode)
	}
	if r := doJSON(t, "DELETE", base+"/v1/schedules/"+id, "", "", nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", r.StatusCode)
	}
	if r := doJSON(t, "GET", base+"/v1/schedules/"+id, "", "", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("get deleted status = %d", r.StatusCode)
	}
}

func TestScheduleDisabledReturns404(t *testing.T) {
	m, _, _ := newTestManager(t)
	ts := httptest.NewServer(NewServer(ServerOptions{Manager: m, DisableWebUI: true}))
	defer ts.Close()
	if r := doJSON(t, "GET", ts.URL+"/v1/schedules", "", "", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled list status = %d", r.StatusCode)
	}
}

func TestScheduleWebSocketOps(t *testing.T) {
	m, _, _ := newTestManager(t)
	svc, _ := newScheduleTestService(t, m)
	ts := httptest.NewServer(NewServer(ServerOptions{Manager: m, Schedules: svc, DisableWebUI: true}))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws"
	conn, err := websocket.Dial(wsURL, "", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	reply := func(id, op string, extra map[string]any) WSMessage {
		frame := map[string]any{"id": id, "op": op}
		for k, v := range extra {
			frame[k] = v
		}
		if err := websocket.JSON.Send(conn, frame); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		_ = conn.SetReadDeadline(deadline)
		for {
			var msg WSMessage
			if err := websocket.JSON.Receive(conn, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Kind == "reply" && msg.ID == id {
				return msg
			}
		}
	}

	created := reply("1", "create_schedule", map[string]any{"params": map[string]any{
		"name": "WS 任务", "prompt": "检查", "trigger": map[string]any{"type": "once", "in": "1h"},
		"timezone": "UTC",
	}})
	if !created.OK {
		t.Fatalf("ws create = %+v", created)
	}
	data, ok := created.Data.(map[string]any)
	if !ok || data["scheduler"] != "active" {
		t.Fatalf("ws create data = %+v", created.Data)
	}
	sch := data["schedule"].(map[string]any)
	id := sch["id"].(string)

	listed := reply("2", "list_schedules", nil)
	if !listed.OK {
		t.Fatalf("ws list = %+v", listed)
	}
	paused := reply("3", "pause_schedule", map[string]any{"schedule_id": id})
	if !paused.OK {
		t.Fatalf("ws pause = %+v", paused)
	}
	validated := reply("4", "validate_trigger", map[string]any{"params": map[string]any{
		"trigger": map[string]any{"type": "interval", "every": "1h"}, "timezone": "UTC",
	}})
	if !validated.OK {
		t.Fatalf("ws validate = %+v", validated)
	}
}

func TestOutcomeFromSummary(t *testing.T) {
	cases := []struct {
		status TurnStatus
		want   schedule.RunStatus
	}{
		{TurnCompleted, schedule.RunCompleted},
		{TurnCancelled, schedule.RunCancelled},
		{TurnTimedOut, schedule.RunTimedOut},
		{TurnFailed, schedule.RunFailed},
	}
	for _, c := range cases {
		out := outcomeFromSummary(&TurnSummary{Status: c.status, FinalOutput: "ok", Usage: &agent.Usage{InputTokens: 1}})
		if out.Status != c.want || out.Usage == nil {
			t.Fatalf("status %s → %+v", c.status, out)
		}
	}
}
