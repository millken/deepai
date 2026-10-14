package figmabridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	listenAddr     = "127.0.0.1:43187"
	requestMaxByte = 8 << 20
	pollFreshSecs  = 30
	waitInterval   = 250 * time.Millisecond
)

type Job struct {
	ID           string `json:"id"`
	TargetNodeID string `json:"targetNodeId,omitempty"`
	FileKey      string `json:"fileKey"`
	NodeID       string `json:"nodeId"`
	State        string `json:"state"`
	SubmittedAt  string `json:"submittedAt"`
	FinishedAt   string `json:"finishedAt,omitempty"`
}

type JobResult struct {
	ID          string          `json:"id"`
	OK          bool            `json:"ok"`
	Output      json.RawMessage `json:"output,omitempty"`
	Before      json.RawMessage `json:"before,omitempty"`
	After       json.RawMessage `json:"after,omitempty"`
	Error       string          `json:"error,omitempty"`
	ExecStarted bool            `json:"executionStarted,omitempty"`
	Preview     string          `json:"preview,omitempty"`
	FinishedAt  string          `json:"finishedAt"`
}

type StatusReport struct {
	Port         int      `json:"port"`
	Connected    bool     `json:"connected"`
	LastPollAgo  string   `json:"lastPollAgo,omitempty"`
	Binding      *Binding `json:"binding,omitempty"`
	Active       *Job     `json:"activeJob,omitempty"`
	Unresolved   []string `json:"unresolvedJobs,omitempty"`
	ManifestPath string   `json:"manifestPath,omitempty"`
}

type Bridge struct {
	mu         sync.Mutex
	dir        string
	token      string
	binding    Binding
	manifest   string
	port       int
	server     *http.Server
	jobs       map[string]*Job
	unresolved map[string]bool
	lastPoll   time.Time
}

var (
	mu           sync.Mutex
	activeBridge *Bridge
)

func (b *Bridge) runDir(id string) string { return filepath.Join(b.dir, "runs", id) }

func (b *Bridge) RunDir(id string) string { return b.runDir(id) }

// New 构造桥并恢复中断现场，不监听端口（测试可挂 httptest）。
func New(dir string, binding Binding, token string) (*Bridge, error) {
	b := &Bridge{
		dir: dir, token: token, binding: binding,
		jobs: map[string]*Job{}, unresolved: map[string]bool{},
	}
	if err := b.recoverRuns(); err != nil {
		return nil, err
	}
	return b, nil
}

// ListenAndServe 监听回环端口并生成插件文件。
func (b *Bridge) ListenAndServe() error {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("端口 %s 被占用：若本机正在运行其他 deepai 实例或旧桥接，请先停止后重试", listenAddr)
	}
	b.port = listener.Addr().(*net.TCPAddr).Port
	manifest, err := GeneratePlugin(b.dir, b.port, b.token, b.binding)
	if err != nil {
		listener.Close()
		return err
	}
	b.manifest = manifest
	b.server = &http.Server{Handler: b}
	go func() { _ = b.server.Serve(listener) }()
	return nil
}

// Get 返回进程内单例桥：加载绑定、配对并监听。
func Get() (*Bridge, error) {
	mu.Lock()
	defer mu.Unlock()
	if activeBridge != nil {
		return activeBridge, nil
	}
	dir, err := StateDir()
	if err != nil {
		return nil, err
	}
	binding, err := loadBinding(dir)
	if err != nil {
		return nil, err
	}
	token, err := PairingToken(dir)
	if err != nil {
		return nil, err
	}
	b, err := New(dir, binding, token)
	if err != nil {
		return nil, err
	}
	if err := b.ListenAndServe(); err != nil {
		return nil, err
	}
	activeBridge = b
	return b, nil
}

// Init 绑定目标（重复绑定同目标幂等）并启动桥。
func Init(rawURL string) (*Bridge, error) {
	binding, err := ParseFigmaURL(rawURL)
	if err != nil {
		return nil, err
	}
	dir, err := StateDir()
	if err != nil {
		return nil, err
	}
	bpath := filepath.Join(dir, "binding.json")
	if existing, err := loadBinding(dir); err == nil {
		if existing.FileKey != binding.FileKey || existing.NodeID != binding.NodeID {
			return nil, fmt.Errorf("已绑定其他目标（%s node %s）；切换请先删除 %s 并在 Figma 重新导入插件", existing.FileKey, existing.NodeID, bpath)
		}
	} else if err := writeJSONAtomic(bpath, binding, 0o600); err != nil {
		return nil, err
	}
	return Get()
}

// recoverRuns 重建中断现场：已领取未回传的任务进入 unresolved（阻断写入，
// 待核实后 ack）；从未领取的任务作废。磁盘是唯一权威证据。
func (b *Bridge) recoverRuns() error {
	runsDir := filepath.Join(b.dir, "runs")
	entries, err := os.ReadDir(runsDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !jobIDRe.MatchString(e.Name()) {
			continue
		}
		jpath := filepath.Join(runsDir, e.Name(), "job.json")
		var job Job
		if err := readJSON(jpath, &job); err != nil {
			return fmt.Errorf("任务记录损坏 %s：%w", jpath, err)
		}
		if _, err := os.Stat(filepath.Join(runsDir, e.Name(), "result.json")); err == nil {
			continue
		}
		switch job.State {
		case "delivered":
			b.jobs[job.ID] = &job
			b.unresolved[job.ID] = true
		case "queued":
			job.State = "cancelled"
			job.FinishedAt = nowISO()
			if err := writeJSONAtomic(jpath, job, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

var jobIDRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

// ---- 插件 HTTP 面 ----

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type,X-Session-Token")
	w.Header().Set("Access-Control-Allow-Methods", "POST,OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Session-Token")), []byte(b.token)) != 1 {
		reply(w, http.StatusForbidden, map[string]any{"error": "认证失败：请重新导入插件"})
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/poll":
		b.handlePoll(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/complete":
		b.handleComplete(w, r)
	default:
		reply(w, http.StatusNotFound, map[string]any{"error": "未知接口"})
	}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (b *Bridge) handlePoll(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Client string `json:"client"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		reply(w, http.StatusBadRequest, map[string]any{"error": "请求体无效"})
		return
	}
	b.mu.Lock()
	b.lastPoll = time.Now()
	var deliver *Job
	for _, job := range b.jobs {
		if job.State == "queued" {
			deliver = job
			break
		}
	}
	if deliver != nil {
		deliver.State = "delivered"
		if err := writeJSONAtomic(filepath.Join(b.runDir(deliver.ID), "job.json"), *deliver, 0o600); err != nil {
			deliver.State = "queued"
			b.mu.Unlock()
			reply(w, http.StatusInternalServerError, map[string]any{"error": "任务落盘失败：" + err.Error()})
			return
		}
	}
	b.mu.Unlock()
	if deliver == nil {
		reply(w, http.StatusOK, nil)
		return
	}
	code, err := os.ReadFile(filepath.Join(b.runDir(deliver.ID), "script.js"))
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]any{"error": "脚本读取失败：" + err.Error()})
		return
	}
	reply(w, http.StatusOK, map[string]any{
		"id":           deliver.ID,
		"code":         string(code),
		"targetNodeId": deliver.TargetNodeID,
		"binding":      map[string]string{"fileKey": b.binding.FileKey, "nodeId": b.binding.NodeID},
	})
}

func (b *Bridge) handleComplete(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, requestMaxByte)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		reply(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "结果超过 8MB 上限"})
		return
	}
	var msg struct {
		ID          string          `json:"id"`
		OK          bool            `json:"ok"`
		Output      json.RawMessage `json:"output"`
		Before      json.RawMessage `json:"before"`
		After       json.RawMessage `json:"after"`
		Error       string          `json:"error"`
		ExecStarted bool            `json:"executionStarted"`
		PNG         []float64       `json:"png"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil || !jobIDRe.MatchString(msg.ID) {
		reply(w, http.StatusBadRequest, map[string]any{"error": "结果无效"})
		return
	}
	b.mu.Lock()
	job := b.jobs[msg.ID]
	complete := job != nil && job.State == "delivered"
	b.mu.Unlock()
	if !complete {
		reply(w, http.StatusConflict, map[string]any{"error": "任务不在待回传状态"})
		return
	}
	result := JobResult{
		ID: msg.ID, OK: msg.OK, Output: msg.Output, Before: msg.Before, After: msg.After,
		Error: msg.Error, ExecStarted: msg.ExecStarted, FinishedAt: nowISO(),
	}
	if msg.OK && len(msg.PNG) > 0 {
		png := make([]byte, len(msg.PNG))
		for i, v := range msg.PNG {
			if v < 0 || v > 255 || v != float64(int(v)) {
				reply(w, http.StatusBadRequest, map[string]any{"error": "图像数据无效"})
				return
			}
			png[i] = byte(int(v))
		}
		if err := os.MkdirAll(b.runDir(job.ID), 0o700); err != nil {
			reply(w, http.StatusInternalServerError, map[string]any{"error": "证据目录创建失败：" + err.Error()})
			return
		}
		if err := os.WriteFile(filepath.Join(b.runDir(job.ID), "preview.png"), png, 0o600); err != nil {
			reply(w, http.StatusInternalServerError, map[string]any{"error": "预览图落盘失败：" + err.Error()})
			return
		}
		result.Preview = "preview.png"
	}
	// 先 result 后 job：任一缺失即视为未完成，恢复扫描据此阻断。
	if err := writeJSONAtomic(filepath.Join(b.runDir(job.ID), "result.json"), result, 0o600); err != nil {
		reply(w, http.StatusInternalServerError, map[string]any{"error": "结果落盘失败：" + err.Error()})
		return
	}
	b.mu.Lock()
	job.State = map[bool]string{true: "done", false: "failed"}[msg.OK]
	job.FinishedAt = result.FinishedAt
	err = writeJSONAtomic(filepath.Join(b.runDir(job.ID), "job.json"), *job, 0o600)
	b.mu.Unlock()
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]any{"error": "任务状态落盘失败：" + err.Error()})
		return
	}
	reply(w, http.StatusOK, map[string]any{"received": true})
}

// ---- 内部 API（figma 工具调用，磁盘证据为权威）----

type SubmitError struct {
	Msg        string
	Unresolved []string
}

func (e *SubmitError) Error() string { return e.Msg }

func (b *Bridge) Submit(code, targetNodeID string) (*Job, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("脚本不能为空")
	}
	if targetNodeID != "" && !targetIDRe.MatchString(targetNodeID) {
		return nil, fmt.Errorf("目标节点 ID 需形如 1:2（收到 %q）", targetNodeID)
	}
	b.mu.Lock()
	if len(b.unresolved) > 0 {
		ids := make([]string, 0, len(b.unresolved))
		for id := range b.unresolved {
			ids = append(ids, id)
		}
		b.mu.Unlock()
		return nil, &SubmitError{Msg: fmt.Sprintf(
			"上次任务 %s 结果未知，已阻断新写入：请先用 figma run 提交只读脚本核实现状，再 figma ack <job_id> <核实说明> 解锁", strings.Join(ids, ", ")),
			Unresolved: ids}
	}
	for _, job := range b.jobs {
		if job.State == "queued" || job.State == "delivered" {
			b.mu.Unlock()
			return nil, fmt.Errorf("任务 %s 尚未完成（%s）：请先读回结果（result %s）", job.ID, job.State, job.ID)
		}
	}
	idBuf := make([]byte, 16)
	if _, err := rand.Read(idBuf); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	job := &Job{
		ID: hex.EncodeToString(idBuf), TargetNodeID: targetNodeID,
		FileKey: b.binding.FileKey, NodeID: b.binding.NodeID,
		State: "queued", SubmittedAt: nowISO(),
	}
	dir := b.runDir(job.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "script.js"), []byte(code), 0o600); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if err := writeJSONAtomic(filepath.Join(dir, "job.json"), *job, 0o600); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	b.jobs[job.ID] = job
	b.mu.Unlock()
	return job, nil
}

var targetIDRe = regexp.MustCompile(`^[0-9I][0-9:;]*$`)

type WaitOutcome struct {
	WaitStatus  string     // completed | timeout
	Job         Job        `json:"job"`
	Result      *JobResult `json:"result,omitempty"`
	Instruction string     `json:"instruction,omitempty"`
}

// Wait 只轮询磁盘证据（result.json 出现即完成），不依赖内存状态。
func (b *Bridge) Wait(ctx context.Context, id string, timeout time.Duration) WaitOutcome {
	deadline := time.Now().Add(timeout)
	for {
		if result, err := b.Result(id); err == nil {
			job, _ := b.JobByID(id)
			return WaitOutcome{WaitStatus: "completed", Job: job, Result: result}
		}
		if time.Now().After(deadline) {
			job, _ := b.JobByID(id)
			return WaitOutcome{WaitStatus: "timeout", Job: job,
				Instruction: fmt.Sprintf("任务 %s 未在超时内回传（未被取消）：用 result %s 读回，或稍后再试；不要重复提交同一修改", id, id)}
		}
		select {
		case <-ctx.Done():
			job, _ := b.JobByID(id)
			return WaitOutcome{WaitStatus: "cancelled", Job: job,
				Instruction: fmt.Sprintf("等待被中断（任务 %s 未被取消）：用 result %s 读回；不要重复提交同一修改", id, id)}
		case <-time.After(waitInterval):
		}
	}
}

func (b *Bridge) Result(id string) (*JobResult, error) {
	if !jobIDRe.MatchString(id) {
		return nil, fmt.Errorf("任务 ID 无效")
	}
	var result JobResult
	if err := readJSON(filepath.Join(b.runDir(id), "result.json"), &result); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("任务 %s 暂无结果", id)
		}
		return nil, err
	}
	return &result, nil
}

func (b *Bridge) JobByID(id string) (Job, error) {
	var job Job
	err := readJSON(filepath.Join(b.runDir(id), "job.json"), &job)
	return job, err
}

func (b *Bridge) PreviewPath(id string) string {
	p := filepath.Join(b.runDir(id), "preview.png")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func (b *Bridge) Ack(id, note string) error {
	if strings.TrimSpace(note) == "" {
		return fmt.Errorf("ack 需要附带核实说明（当前文档现状与影响判断）")
	}
	b.mu.Lock()
	if !b.unresolved[id] {
		b.mu.Unlock()
		return fmt.Errorf("任务 %s 不在待核实状态", id)
	}
	delete(b.unresolved, id)
	job, hasJob := b.jobs[id]
	if hasJob {
		job.State = "acknowledged"
	}
	var err error
	if hasJob {
		err = writeJSONAtomic(filepath.Join(b.runDir(id), "job.json"), *job, 0o600)
	}
	b.mu.Unlock()
	if err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(b.runDir(id), "ack.json"),
		map[string]any{"id": id, "note": note, "at": nowISO()}, 0o600)
}

func (b *Bridge) History() []Job {
	runsDir := filepath.Join(b.dir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return nil
	}
	var jobs []Job
	for _, e := range entries {
		if !e.IsDir() || !jobIDRe.MatchString(e.Name()) {
			continue
		}
		if job, err := b.JobByID(e.Name()); err == nil {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

func (b *Bridge) Status() StatusReport {
	b.mu.Lock()
	defer b.mu.Unlock()
	report := StatusReport{
		Port:         b.port,
		Binding:      &b.binding,
		ManifestPath: b.manifest,
		Connected:    !b.lastPoll.IsZero() && time.Since(b.lastPoll) < pollFreshSecs*time.Second,
	}
	if !b.lastPoll.IsZero() {
		report.LastPollAgo = time.Since(b.lastPoll).Round(time.Millisecond).String()
	}
	for _, job := range b.jobs {
		if job.State == "queued" || job.State == "delivered" {
			j := *job
			report.Active = &j
		}
		if b.unresolved[job.ID] {
			report.Unresolved = append(report.Unresolved, job.ID)
		}
	}
	return report
}
