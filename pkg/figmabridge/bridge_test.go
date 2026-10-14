package figmabridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestBridge(t *testing.T) *Bridge {
	t.Helper()
	dir := t.TempDir()
	binding := Binding{ID: "b1", SourceURL: "https://www.figma.com/design/KEY/f?node-id=1-2", FileKey: "KEY", NodeID: "1:2", BoundAt: nowISO()}
	b, err := New(dir, binding, "testtoken")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if b.server != nil {
			_ = b.server.Close()
		}
	})
	return b
}

func pollAs(t *testing.T, b *Bridge, token string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("POST", "/poll", strings.NewReader(`{"client":"p1"}`))
	req.Header.Set("X-Session-Token", token)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("poll status %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func completeAs(t *testing.T, b *Bridge, token, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req, _ := http.NewRequest("POST", "/complete", strings.NewReader(payload))
	req.Header.Set("X-Session-Token", token)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	return rec
}

func TestBridge_AuthReject(t *testing.T) {
	b := newTestBridge(t)
	req, _ := http.NewRequest("POST", "/poll", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestBridge_JobLifecycle(t *testing.T) {
	b := newTestBridge(t)
	job, err := b.Submit("return 1+1", "")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" {
		t.Fatalf("state = %s", job.State)
	}

	// 单任务串行：active 未决时拒绝新提交。
	if _, err := b.Submit("return 2", ""); err == nil || !strings.Contains(err.Error(), "尚未完成") {
		t.Fatalf("want single-flight rejection, got %v", err)
	}

	got := pollAs(t, b, "testtoken")
	if got["id"] != job.ID || got["code"] != "return 1+1" {
		t.Fatalf("poll delivered %+v", got)
	}
	if got["binding"].(map[string]any)["fileKey"] != "KEY" {
		t.Fatalf("binding missing in delivery")
	}
	// 已领取不重复交付。
	if again := pollAs(t, b, "testtoken"); again != nil {
		t.Fatalf("double delivery: %+v", again)
	}

	payload := fmt.Sprintf(`{"id":%q,"ok":true,"output":2,"before":{"id":"1:2"},"after":{"id":"1:2"}}`, job.ID)
	if rec := completeAs(t, b, "testtoken", payload); rec.Code != 200 {
		t.Fatalf("complete status %d: %s", rec.Code, rec.Body.String())
	}
	// 重复 complete 幂等拒绝。
	if rec := completeAs(t, b, "testtoken", payload); rec.Code != 409 {
		t.Fatalf("duplicate complete want 409, got %d", rec.Code)
	}

	result, err := b.Result(job.ID)
	if err != nil || !result.OK {
		t.Fatalf("result = %+v, %v", result, err)
	}
	// 完成后可提交新任务。
	if _, err := b.Submit("return 3", ""); err != nil {
		t.Fatalf("submit after completion: %v", err)
	}
}

func TestBridge_PreviewPNG(t *testing.T) {
	b := newTestBridge(t)
	job, _ := b.Submit("return await target.exportAsync({format:'PNG'})", "")
	pollAs(t, b, "testtoken")
	png := []byte{0x89, 0x50, 0x4E, 0x47}
	arr := make([]string, len(png))
	for i, v := range png {
		arr[i] = fmt.Sprintf("%d", v)
	}
	payload := fmt.Sprintf(`{"id":%q,"ok":true,"png":[%s]}`, job.ID, strings.Join(arr, ","))
	if rec := completeAs(t, b, "testtoken", payload); rec.Code != 200 {
		t.Fatalf("complete status %d: %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(b.PreviewPath(job.ID))
	if err != nil || !bytes.Equal(data, png) {
		t.Fatalf("preview = %v, %v", data, err)
	}
	// 非法字节拒绝。
	job2, _ := b.Submit("x", "")
	pollAs(t, b, "testtoken")
	if rec := completeAs(t, b, "testtoken", fmt.Sprintf(`{"id":%q,"ok":true,"png":[300]}`, job2.ID)); rec.Code != 400 {
		t.Fatalf("invalid png want 400, got %d", rec.Code)
	}
}

func TestBridge_WaitCompletes(t *testing.T) {
	b := newTestBridge(t)
	job, _ := b.Submit("return 42", "")
	go func() {
		time.Sleep(100 * time.Millisecond)
		pollAs(t, b, "testtoken")
		completeAs(t, b, "testtoken", fmt.Sprintf(`{"id":%q,"ok":true,"output":42}`, job.ID))
	}()
	outcome := b.Wait(t.Context(), job.ID, 5*time.Second)
	if outcome.WaitStatus != "completed" || outcome.Result == nil || string(outcome.Result.Output) != "42" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestBridge_WaitTimeout(t *testing.T) {
	b := newTestBridge(t)
	job, _ := b.Submit("slow", "")
	outcome := b.Wait(t.Context(), job.ID, 300*time.Millisecond)
	if outcome.WaitStatus != "timeout" || outcome.Instruction == "" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// delivered 无 result = 不确定态：重启恢复后进入 unresolved，阻断写入，ack 后解锁。
func TestBridge_RecoveryAndAck(t *testing.T) {
	dir := t.TempDir()
	binding := Binding{ID: "b1", FileKey: "KEY", NodeID: "1:2"}
	b1, err := New(dir, binding, "tok")
	if err != nil {
		t.Fatal(err)
	}
	job, _ := b1.Submit("mutate", "")
	pollAs(t, b1, "tok") // delivered

	// 模拟进程中断：同一磁盘重建桥。
	b2, err := New(dir, binding, "tok")
	if err != nil {
		t.Fatal(err)
	}
	status := b2.Status()
	if len(status.Unresolved) != 1 || status.Unresolved[0] != job.ID {
		t.Fatalf("unresolved = %v", status.Unresolved)
	}
	if _, err := b2.Submit("write", ""); err == nil {
		t.Fatal("write must be blocked while unresolved")
	} else if !strings.Contains(err.Error(), "ack") {
		t.Fatalf("blocked message should mention ack: %v", err)
	}
	// 失败任务释放串行锁（无需 ack——ok=false 是确定终态）。
	if err := b2.Ack(job.ID, ""); err == nil {
		t.Fatal("ack without note must fail")
	}
	if err := b2.Ack(job.ID, "inspect 确认仅按钮文字变化"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if _, err := b2.Submit("write", ""); err != nil {
		t.Fatalf("write after ack: %v", err)
	}
	if err := b2.Ack(job.ID, "again"); err == nil {
		t.Fatal("double ack must fail")
	}
	ackData, _ := os.ReadFile(filepath.Join(dir, "runs", job.ID, "ack.json"))
	if !strings.Contains(string(ackData), "仅按钮文字变化") {
		t.Fatalf("ack note not persisted: %s", ackData)
	}
}

func TestBridge_QueueCancelledOnRecovery(t *testing.T) {
	dir := t.TempDir()
	binding := Binding{ID: "b1", FileKey: "KEY", NodeID: "1:2"}
	b1, _ := New(dir, binding, "tok")
	job, _ := b1.Submit("never-delivered", "")
	_ = job
	b2, err := New(dir, binding, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if status := b2.Status(); status.Active != nil {
		t.Fatalf("queued job should be cancelled, got active %+v", status.Active)
	}
	if jobs := b2.History(); len(jobs) != 1 || jobs[0].State != "cancelled" {
		t.Fatalf("history = %+v", b2.History())
	}
}

func TestParseFigmaURL(t *testing.T) {
	ok := "https://www.figma.com/design/KEY7/File-Name?node-id=12-34&t=x"
	binding, err := ParseFigmaURL(ok)
	if err != nil || binding.FileKey != "KEY7" || binding.NodeID != "12:34" {
		t.Fatalf("parse = %+v, %v", binding, err)
	}
	for _, bad := range []string{
		"https://www.figma.com/design/KEY/f",            // 无 node-id
		"http://www.figma.com/design/KEY/f?node-id=1-2", // 非 https
		"https://evil.com/design/KEY/f?node-id=1-2",     // 非法域名
		"https://www.figma.com/file/KEY/f?node-id=abc",  // 非法 node-id
		"not a url",
	} {
		if _, err := ParseFigmaURL(bad); err == nil {
			t.Errorf("ParseFigmaURL(%q) should fail", bad)
		}
	}
}

func TestPairingTokenStable(t *testing.T) {
	dir := t.TempDir()
	t1, err := PairingToken(dir)
	if err != nil || len(t1) != 64 {
		t.Fatalf("t1 = %q, %v", t1, err)
	}
	t2, err := PairingToken(dir)
	if err != nil || t2 != t1 {
		t.Fatalf("pairing token must persist: %q vs %q", t1, t2)
	}
}

func TestGeneratePlugin(t *testing.T) {
	dir := t.TempDir()
	binding := Binding{FileKey: "KEY", NodeID: "9:9"}
	manifest, err := GeneratePlugin(dir, 43187, "tok123", binding)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != "deepai Figma Bridge" || m["documentAccess"] != "dynamic-page" {
		t.Fatalf("manifest = %s", data)
	}
	ui, _ := os.ReadFile(filepath.Join(dir, "plugin", "ui.html"))
	if !strings.Contains(string(ui), `"token":"tok123"`) || !strings.Contains(string(ui), `"fileKey":"KEY"`) {
		t.Fatal("ui.html must embed session config")
	}
	if strings.Contains(string(ui), "SESSION_CONFIG") {
		t.Fatal("SESSION_CONFIG placeholder must be replaced")
	}
	if _, err := os.Stat(filepath.Join(dir, "plugin", "main.js")); err != nil {
		t.Fatal(err)
	}
}

func TestBridge_FailedResultCarriesEvidence(t *testing.T) {
	b := newTestBridge(t)
	job, _ := b.Submit("throw Error('boom')", "")
	pollAs(t, b, "testtoken")
	payload := fmt.Sprintf(`{"id":%q,"ok":false,"error":"Error: boom","executionStarted":true,"after":{"id":"1:2"}}`, job.ID)
	if rec := completeAs(t, b, "testtoken", payload); rec.Code != 200 {
		t.Fatalf("complete status %d", rec.Code)
	}
	result, _ := b.Result(job.ID)
	if result.OK || result.Error != "Error: boom" || !result.ExecStarted {
		t.Fatalf("result = %+v", result)
	}
	if outcome := b.Wait(t.Context(), job.ID, time.Second); outcome.WaitStatus != "completed" {
		t.Fatalf("failed job is still a completed wait: %+v", outcome)
	}
	// 失败任务释放串行锁（无需 ack——ok=false 是确定终态）。
	if _, err := b.Submit("next", ""); err != nil {
		t.Fatalf("failed job must release lock: %v", err)
	}
}
