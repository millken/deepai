package figmabridge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	job, err := b.Submit("return 1+1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" {
		t.Fatalf("state = %s", job.State)
	}

	// 单任务串行：active 未决时拒绝新提交。
	if _, err := b.Submit("return 2", "", false); err == nil || !strings.Contains(err.Error(), "尚未完成") {
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
	if _, err := b.Submit("return 3", "", false); err != nil {
		t.Fatalf("submit after completion: %v", err)
	}
}

func TestBridge_PreviewPNG(t *testing.T) {
	b := newTestBridge(t)
	job, _ := b.Submit("return await target.exportAsync({format:'PNG'})", "", false)
	pollAs(t, b, "testtoken")
	png := []byte{0x89, 0x50, 0x4E, 0x47}
	payload := fmt.Sprintf(`{"id":%q,"ok":true,"pngB64":%q,"before":{"id":"1:2"},"after":{"id":"1:2"}}`, job.ID, base64.StdEncoding.EncodeToString(png))
	if rec := completeAs(t, b, "testtoken", payload); rec.Code != 200 {
		t.Fatalf("complete status %d: %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(b.PreviewPath(job.ID))
	if err != nil || !bytes.Equal(data, png) {
		t.Fatalf("preview = %v, %v", data, err)
	}
	result, _ := b.Result(job.ID)
	if len(result.Before) == 0 || len(result.After) == 0 {
		t.Fatalf("png result must still carry before/after: %+v", result)
	}
	job2, _ := b.Submit("x", "", false)
	pollAs(t, b, "testtoken")
	if rec := completeAs(t, b, "testtoken", fmt.Sprintf(`{"id":%q,"ok":true,"pngB64":"!!!"}`, job2.ID)); rec.Code != 400 {
		t.Fatalf("invalid png want 400, got %d", rec.Code)
	}
}

func TestBridge_WaitCompletes(t *testing.T) {
	b := newTestBridge(t)
	job, _ := b.Submit("return 42", "", false)
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
	job, _ := b.Submit("slow", "", false)
	outcome := b.Wait(t.Context(), job.ID, 300*time.Millisecond)
	if outcome.WaitStatus != "timeout" || outcome.Instruction == "" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// delivered 无 result = 不确定态：重启恢复后进入 unresolved。完整解锁流程：
// 普通 run 被拒 → inspect run 放行并完成 → 未 ack 前写入仍被拒 → ack（带核实任务）→ 放行。
func TestBridge_RecoveryAndAck(t *testing.T) {
	dir := t.TempDir()
	binding := Binding{ID: "b1", FileKey: "KEY", NodeID: "1:2"}
	b1, err := New(dir, binding, "tok")
	if err != nil {
		t.Fatal(err)
	}
	job, _ := b1.Submit("mutate", "", false)
	pollAs(t, b1, "tok") // delivered

	b2, err := New(dir, binding, "tok")
	if err != nil {
		t.Fatal(err)
	}
	status := b2.Status()
	if len(status.Unresolved) != 1 || status.Unresolved[0] != job.ID {
		t.Fatalf("unresolved = %v", status.Unresolved)
	}
	if _, err := b2.Submit("write", "", false); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("normal run must be blocked with inspect hint: %v", err)
	}
	if err := b2.Ack(job.ID, "", "未核实"); err == nil {
		t.Fatal("ack without inspect job must fail")
	}
	// 非inspect 任务不能冒充核实依据（M1：校验 Inspect 标记，用被阻断任务自身 Inspect=false 验证）。
	if err := b2.Ack(job.ID, job.ID, "自救"); err == nil || !strings.Contains(err.Error(), "inspect:true") {
		t.Fatalf("ack with non-inspect job must fail: %v", err)
	}
	inspectJob, err := b2.Submit("return null", "", true)
	if err != nil {
		t.Fatalf("inspect run during unresolved: %v", err)
	}
	if err := b2.Ack(job.ID, inspectJob.ID, "过早"); err == nil || !strings.Contains(err.Error(), "尚无结果") {
		t.Fatalf("ack before inspect completes must fail: %v", err)
	}
	pollAs(t, b2, "tok")
	completeAs(t, b2, "tok", fmt.Sprintf(`{"id":%q,"ok":true,"output":null}`, inspectJob.ID))
	if _, err := b2.Submit("write", "", false); err == nil {
		t.Fatal("write must stay blocked until ack")
	}
	if err := b2.Ack(job.ID, inspectJob.ID, ""); err == nil {
		t.Fatal("ack without note must fail")
	}
	if err := b2.Ack(job.ID, inspectJob.ID, "inspect 确认仅按钮文字变化"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if _, err := b2.Submit("write", "", false); err != nil {
		t.Fatalf("write after ack: %v", err)
	}
	if err := b2.Ack(job.ID, inspectJob.ID, "again"); err == nil {
		t.Fatal("double ack must fail")
	}
	ackData, _ := os.ReadFile(filepath.Join(dir, "runs", job.ID, "ack.json"))
	if !strings.Contains(string(ackData), "仅按钮文字变化") || !strings.Contains(string(ackData), inspectJob.ID) {
		t.Fatalf("ack note not persisted: %s", ackData)
	}
}

// 换目标：在途任务拒绝；空闲时内存绑定、插件下发与 binding.json 同步更新。
func TestBridge_Retarget(t *testing.T) {
	b := newTestBridge(t)
	if err := b.Retarget(Binding{FileKey: "KEY", NodeID: "1:2"}); err != nil {
		t.Fatalf("same target: %v", err)
	}
	job, _ := b.Submit("pending", "", false)
	if err := b.Retarget(Binding{FileKey: "OTHER", NodeID: "9:9"}); err == nil || !strings.Contains(err.Error(), "尚未完成") {
		t.Fatalf("retarget with active job must fail: %v", err)
	}
	pollAs(t, b, "testtoken")
	completeAs(t, b, "testtoken", fmt.Sprintf(`{"id":%q,"ok":true,"output":null}`, job.ID))
	if err := b.Retarget(Binding{FileKey: "OTHER", NodeID: "9:9"}); err != nil {
		t.Fatalf("retarget idle: %v", err)
	}
	if b.Status().Binding.FileKey != "OTHER" {
		t.Fatalf("binding not updated: %+v", b.Status().Binding)
	}
	var onDisk Binding
	if err := readJSON(filepath.Join(b.dir, "binding.json"), &onDisk); err != nil || onDisk.FileKey != "OTHER" {
		t.Fatalf("binding.json = %+v, %v", onDisk, err)
	}
	_, _ = b.Submit("next", "", false)
	delivered := pollAs(t, b, "testtoken")
	if delivered["binding"].(map[string]any)["fileKey"] != "OTHER" {
		t.Fatalf("poll binding = %v", delivered["binding"])
	}
}

func TestBridge_QueueCancelledOnRecovery(t *testing.T) {
	dir := t.TempDir()
	binding := Binding{ID: "b1", FileKey: "KEY", NodeID: "1:2"}
	b1, _ := New(dir, binding, "tok")
	job, _ := b1.Submit("never-delivered", "", false)
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
	// Figma 的 manifest 校验器要求 devAllowedDomains 为合法主机名——IP 字面量
	// (http://127.0.0.1:...) 会在导入/运行时报 Invalid value 拒绝(#23 round-2 回归,
	// 2026-10-14 实机复现)。因此 manifest 与 ui fetch 统一用 localhost(回环等价),
	// 仍需同源以过 Figma 的请求 URL 对照。
	domains := m["networkAccess"].(map[string]any)["devAllowedDomains"].([]any)
	if len(domains) != 1 || domains[0] != "http://localhost:43187" {
		t.Fatalf("devAllowedDomains = %v", domains)
	}
	if !strings.Contains(string(ui), "http://localhost:") || strings.Contains(string(ui), "http://127.0.0.1:") {
		t.Fatal("ui.html fetch host must match devAllowedDomains")
	}
	if _, err := os.Stat(filepath.Join(dir, "plugin", "main.js")); err != nil {
		t.Fatal(err)
	}
}

func TestBridge_FailedResultCarriesEvidence(t *testing.T) {
	b := newTestBridge(t)
	job, _ := b.Submit("throw Error('boom')", "", false)
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
	if _, err := b.Submit("next", "", false); err != nil {
		t.Fatalf("failed job must release lock: %v", err)
	}
}
// P0：多图 pngsB64 落盘 preview.png / preview-N.png，Preview 记录清单，PreviewPath 仍指向首图。
func TestBridge_MultiPreviewPNG(t *testing.T) {
	b := newTestBridge(t)
	job, _ := b.Submit("return {a: await t1.exportAsync(), b: await t2.exportAsync()}", "", false)
	pollAs(t, b, "testtoken")
	png1 := []byte{0x89, 0x50, 0x4E, 0x47}
	png2 := []byte{0x89, 0x50, 0x4E, 0x47, 0x01}
	payload := fmt.Sprintf(`{"id":%q,"ok":true,"pngsB64":[%q,%q],"before":{"id":"1:2"},"after":{"id":"1:2"}}`,
		job.ID, base64.StdEncoding.EncodeToString(png1), base64.StdEncoding.EncodeToString(png2))
	if rec := completeAs(t, b, "testtoken", payload); rec.Code != 200 {
		t.Fatalf("complete status %d: %s", rec.Code, rec.Body.String())
	}
	data1, err := os.ReadFile(filepath.Join(b.RunDir(job.ID), "preview.png"))
	if err != nil || !bytes.Equal(data1, png1) {
		t.Fatalf("preview.png = %v, %v", data1, err)
	}
	data2, err := os.ReadFile(filepath.Join(b.RunDir(job.ID), "preview-2.png"))
	if err != nil || !bytes.Equal(data2, png2) {
		t.Fatalf("preview-2.png = %v, %v", data2, err)
	}
	result, _ := b.Result(job.ID)
	if result.Preview != "preview.png,preview-2.png" {
		t.Fatalf("preview list = %q", result.Preview)
	}
	if p := b.PreviewPath(job.ID); p == "" {
		t.Fatal("PreviewPath must still resolve first image")
	}
}

// P1：断连下的超时提示区分「未领取（queued）」与「执行中失联（delivered）」。
func TestBridge_WaitTimeoutDisconnected(t *testing.T) {
	b := newTestBridge(t)
	// 从未 poll：lastPoll 零值 → 未连接，任务停在 queued。
	job, _ := b.Submit("read", "", false)
	outcome := b.Wait(t.Context(), job.ID, 200*time.Millisecond)
	if outcome.WaitStatus != "timeout" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if !strings.Contains(outcome.Instruction, "面板未连接") || !strings.Contains(outcome.Instruction, "未被领取") {
		t.Fatalf("queued-disconnect instruction = %q", outcome.Instruction)
	}

	// 领取后回传前断连：poll 建立连接，再把 lastPoll 拨回过期时刻模拟断连。
	b2 := newTestBridge(t)
	job2, _ := b2.Submit("mutate", "", false)
	pollAs(t, b2, "testtoken")
	b2.mu.Lock()
	b2.lastPoll = time.Now().Add(-pollFreshSecs*time.Second - time.Second)
	b2.mu.Unlock()
	outcome2 := b2.Wait(t.Context(), job2.ID, 200*time.Millisecond)
	if outcome2.WaitStatus != "timeout" || !strings.Contains(outcome2.Instruction, "执行期间断开") ||
		!strings.Contains(outcome2.Instruction, "不会被重放") || !strings.Contains(outcome2.Instruction, "ack") {
		t.Fatalf("delivered-disconnect outcome = %+v", outcome2)
	}
}

// 守卫：嵌入的插件 JS 必须可解析。曾有多余的 "}" 带病合并——go 不解析 JS，
// 测试全绿但插件在 Figma 里加载即失败。GeneratePlugin 落盘后用 node --check
// 校验 main.js 与 ui.html 抽出的内联脚本；node 缺席时跳过。
func TestEmbeddedPluginJSSyntax(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node 不在 PATH，跳过 JS 语法守卫")
	}
	dir := t.TempDir()
	manifest, err := GeneratePlugin(dir, 43187, "tok", Binding{FileKey: "KEY", NodeID: "1:2"})
	if err != nil {
		t.Fatal(err)
	}
	pdir := filepath.Dir(manifest)
	check := func(path string) {
		if out, err := exec.Command("node", "--check", path).CombinedOutput(); err != nil {
			t.Errorf("JS 语法守卫失败 %s: %v\n%s", path, err, out)
		}
	}
	check(filepath.Join(pdir, "main.js"))
	ui, err := os.ReadFile(filepath.Join(pdir, "ui.html"))
	if err != nil {
		t.Fatal(err)
	}
	if o, c := strings.Index(string(ui), "<script>"), strings.LastIndex(string(ui), "</script>"); o >= 0 && c > o {
		tmp := filepath.Join(dir, "ui-inline.js")
		if err := os.WriteFile(tmp, ui[o+len("<script>"):c], 0o600); err != nil {
			t.Fatal(err)
		}
		check(tmp)
	}
}

// 心跳轮询（heartbeat:true）：刷新连接新鲜度但不领取任务——busy 面板执行期
// 持续心跳，避免超过 pollFreshSecs 的执行被误判断连（本 PR 根因）。
func TestBridge_HeartbeatPollNoDelivery(t *testing.T) {
	b := newTestBridge(t)
	job, _ := b.Submit("slow", "", false)
	req, _ := http.NewRequest("POST", "/poll", strings.NewReader(`{"client":"p1","heartbeat":true}`))
	req.Header.Set("X-Session-Token", "testtoken")
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("heartbeat poll status %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, job.ID) {
		t.Fatalf("heartbeat must not deliver jobs: %s", body)
	}
	if job.State != "queued" {
		t.Fatalf("job state = %s, want queued", job.State)
	}
	if !b.Status().Connected {
		t.Fatal("heartbeat must refresh connection freshness")
	}
	// 非心跳轮询正常领取。
	if got := pollAs(t, b, "testtoken"); got["id"] != job.ID {
		t.Fatalf("normal poll must deliver: %+v", got)
	}
}

// 假死收割：delivered 无结果 + 断连超阈值 → Submit 时自动转待核实（免重启）。
// 随后迟到回传（面板复活）自动解除待核实——不再需要 inspect+ack。
func TestBridge_ReapAndLateCompleteUnlock(t *testing.T) {
	b := newTestBridge(t)
	job, _ := b.Submit("mutate", "", false)
	pollAs(t, b, "testtoken") // delivered；此刻 lastPoll 新鲜

	// 断连超过阈值：拨回 lastPoll 后再提交写入 → 被收割进 unresolved 并阻断。
	b.mu.Lock()
	b.lastPoll = time.Now().Add(-reapDisconnect - time.Minute)
	b.mu.Unlock()
	if _, err := b.Submit("write", "", false); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("write must be blocked after reap: %v", err)
	}
	if st := b.Status(); len(st.Unresolved) != 1 || st.Unresolved[0] != job.ID {
		t.Fatalf("unresolved = %v", st.Unresolved)
	}

	// 面板复活后迟到回传：handleComplete 解除 unresolved，无需 ack。
	payload := fmt.Sprintf(`{"id":%q,"ok":true,"output":42}`, job.ID)
	if rec := completeAs(t, b, "testtoken", payload); rec.Code != 200 {
		t.Fatalf("late complete status %d: %s", rec.Code, rec.Body.String())
	}
	if st := b.Status(); len(st.Unresolved) != 0 {
		t.Fatalf("late complete must clear unresolved: %v", st.Unresolved)
	}
	// 回传刷新 lastPoll：否则收割后紧随的 Submit 会因陈旧心跳把同一 id 再收割回去。
	if st := b.Status(); !st.Connected {
		t.Fatal("complete must refresh lastPoll")
	}
	if _, err := b.Submit("next", "", false); err != nil {
		t.Fatalf("write after late complete: %v", err)
	}
}
