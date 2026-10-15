package figmabridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
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
	// delivered 无结果的自动核销阈值：领取它的面板会话静默超过 orphanAfter（面板关闭/重开），
	// 或交付超过 execTimeout（脚本挂死）。核销写入「结果未知」失败结果并释放单飞。
	orphanAfter = 60 * time.Second
	execTimeout = 10 * time.Minute
	// 终态证据保留期：桥启动时清理更早的 run 目录；待核实与在途任务是恢复协议的权威证据，永不清。
	runsRetention = 7 * 24 * time.Hour
	waitInterval  = 250 * time.Millisecond
)

// pollHold 是 wait:true 长轮询的最长挂起时间，须小于插件侧请求超时。
// 长轮询让面板的请求链不经过定时器：Figma 窗口转入后台后 Chromium 会把
// setInterval 节流到每分钟一次，短轮询因此周期性跌出 pollFreshSecs 判定断连。
var pollHold = 20 * time.Second

type Job struct {
	ID           string `json:"id"`
	TargetNodeID string `json:"targetNodeId,omitempty"`
	Inspect      bool   `json:"inspect,omitempty"`
	FileKey      string `json:"fileKey"`
	NodeID       string `json:"nodeId"`
	State        string `json:"state"`
	SubmittedAt  string `json:"submittedAt"`
	DeliveredAt  string `json:"deliveredAt,omitempty"`
	DeliveredTo  string `json:"deliveredTo,omitempty"` // 领取任务的面板会话；旧版面板为空
	FinishedAt   string `json:"finishedAt,omitempty"`

	deliveredAt time.Time
	verifies    []string // inspect 任务提交时待核实的核销任务；该 inspect 成功即解除
}

type panelSession struct {
	lastSeen time.Time
	waiting  int
	busy     bool // 最近一次轮询为执行期心跳
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
	Unverified   []string `json:"expiredUnverifiedJobs,omitempty"`
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
	waiters    int           // 挂起中的长轮询数：>0 即面板在线
	wake       chan struct{} // 有新任务入队时关闭并替换，唤醒挂起的长轮询
	sessions   map[string]*panelSession
	unverified map[string]bool // 已核销、尚未经 inspect 核实的任务：阻断普通写入
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
		wake: make(chan struct{}), sessions: map[string]*panelSession{}, unverified: map[string]bool{},
	}
	if err := b.recoverRuns(); err != nil {
		return nil, err
	}
	b.pruneRuns()
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

// Init 绑定目标并启动桥；活桥存在时安全切换（在途/未决任务拒绝，空闲则热更新）。
func Init(rawURL string) (*Bridge, error) {
	binding, err := ParseFigmaURL(rawURL)
	if err != nil {
		return nil, err
	}
	dir, err := StateDir()
	if err != nil {
		return nil, err
	}
	mu.Lock()
	live := activeBridge
	mu.Unlock()
	if live != nil {
		if err := live.Retarget(binding); err != nil {
			return nil, err
		}
		return live, nil
	}
	bpath := filepath.Join(dir, "binding.json")
	if existing, err := loadBinding(dir); err == nil && existing.FileKey == binding.FileKey && existing.NodeID == binding.NodeID {
		return Get()
	}
	if err := writeJSONAtomic(bpath, binding, 0o600); err != nil {
		return nil, err
	}
	return Get()
}

// Retarget 热切换绑定目标：仅在空闲（无在途任务、无未决核实）时允许，
// 内存绑定、插件与 binding.json 同步更新，任一失败整体回滚。
func (b *Bridge) Retarget(binding Binding) error {
	b.mu.Lock()
	b.expireStaleLocked()
	if b.binding.FileKey == binding.FileKey && b.binding.NodeID == binding.NodeID {
		b.mu.Unlock()
		return nil
	}
	for _, job := range b.jobs {
		// unresolved 的陈旧 delivered 由下方核实检查接管，避免误导去读不存在的 result。
		if (job.State == "queued" || job.State == "delivered") && !b.unresolved[job.ID] {
			id, state := job.ID, job.State
			b.mu.Unlock()
			return fmt.Errorf("任务 %s 尚未完成（%s）：切换目标前先读回结果", id, state)
		}
	}
	if len(b.unresolved) > 0 {
		b.mu.Unlock()
		return fmt.Errorf("存在结果未知任务，需先核实并 ack 后才能切换目标")
	}
	if len(b.unverified) > 0 {
		b.mu.Unlock()
		return fmt.Errorf("存在已核销、结果未知的任务，需先用 inspect:true 核实成功后才能切换目标")
	}
	old := b.binding
	b.binding = binding
	b.mu.Unlock()
	rollback := func() {
		b.mu.Lock()
		b.binding = old
		b.mu.Unlock()
	}
	manifest, err := GeneratePlugin(b.dir, b.port, b.token, binding)
	if err != nil {
		rollback()
		return err
	}
	if err := writeJSONAtomic(filepath.Join(b.dir, "binding.json"), binding, 0o600); err != nil {
		if regen, rerr := GeneratePlugin(b.dir, b.port, b.token, old); rerr == nil {
			b.mu.Lock()
			b.manifest = regen
			b.mu.Unlock()
		}
		rollback()
		return err
	}
	b.mu.Lock()
	b.manifest = manifest
	b.mu.Unlock()
	return nil
}

// runEntries 列出 runs 下形如任务 ID 的子目录；目录不存在视为空。
func (b *Bridge) runEntries() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(b.dir, "runs"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && jobIDRe.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// recoverRuns 重建中断现场：已领取未回传的任务进入 unresolved（阻断写入，
// 待核实后 ack）；从未领取的任务作废。磁盘是唯一权威证据。
func (b *Bridge) recoverRuns() error {
	ids, err := b.runEntries()
	if err != nil {
		return err
	}
	for _, id := range ids {
		jpath := filepath.Join(b.runDir(id), "job.json")
		var job Job
		if err := readJSON(jpath, &job); err != nil {
			return fmt.Errorf("任务记录损坏 %s：%w", jpath, err)
		}
		if _, err := os.Stat(filepath.Join(b.runDir(id), "result.json")); err == nil {
			if job.State == "expired" {
				b.jobs[job.ID] = &job
				b.unverified[job.ID] = true
			}
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

// pruneRuns 清理超过保留期的终态证据目录（含 preview PNG）。终态 = 已有
// result.json（done/failed/含崩溃窗口的 delivered）或 cancelled/acknowledged；
// delivered 无 result 的任务可能仍在待核实，绝不触碰。删除失败静默跳过——
// 清理是尽力的，不能阻断桥启动。
func (b *Bridge) pruneRuns() {
	cutoff := time.Now().Add(-runsRetention)
	ids, err := b.runEntries()
	if err != nil {
		return
	}
	for _, id := range ids {
		dir := b.runDir(id)
		var job Job
		if readJSON(filepath.Join(dir, "job.json"), &job) != nil {
			continue
		}
		_, statErr := os.Stat(filepath.Join(dir, "result.json"))
		hasResult := statErr == nil
		if !hasResult && job.State != "cancelled" && job.State != "acknowledged" {
			continue
		}
		if job.State == "expired" {
			continue
		}
		finished := job.FinishedAt
		if finished == "" && hasResult {
			var res JobResult
			if readJSON(filepath.Join(dir, "result.json"), &res) == nil {
				finished = res.FinishedAt
			}
		}
		t, err := time.Parse(time.RFC3339, finished)
		if err != nil || t.After(cutoff) {
			continue
		}
		_ = os.RemoveAll(dir)
	}
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
		Client    string `json:"client"`
		Heartbeat bool   `json:"heartbeat"` // 执行期心跳：只刷新连接新鲜度，不领取任务
		Wait      bool   `json:"wait"`      // 长轮询：无任务时挂起至多 pollHold，入队即返回
		Session   string `json:"session"`   // 面板实例 ID（每次打开面板随机生成）
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		reply(w, http.StatusBadRequest, map[string]any{"error": "请求体无效"})
		return
	}
	if len(body.Session) > 64 {
		reply(w, http.StatusBadRequest, map[string]any{"error": "会话 ID 无效"})
		return
	}
	var hold <-chan time.Time
	var wake <-chan struct{}
	if body.Wait {
		timer := time.NewTimer(pollHold)
		defer timer.Stop()
		hold = timer.C
		b.mu.Lock()
		b.touchLocked(body.Session, 1)
		// 心跳不查队列、只等通道：在计入挂起的同一把锁内取通道，此后的入队必能唤醒它。
		wake = b.wake
		b.mu.Unlock()
		defer func() {
			b.mu.Lock()
			b.touchLocked(body.Session, -1)
			b.mu.Unlock()
		}()
	}
	for {
		// 面板已断开就不再领取：否则任务被标 delivered 却无人执行。
		if r.Context().Err() != nil {
			return
		}
		delivery, next, err := b.takeJob(body.Heartbeat, body.Session)
		if !body.Heartbeat || wake == nil {
			wake = next
		}
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if delivery != nil || hold == nil {
			reply(w, http.StatusOK, delivery)
			return
		}
		select {
		case <-wake:
			// 心跳轮询被唤醒即返回空：心跳不领取，面板执行完会中止在途心跳、改发领取轮询。
			if body.Heartbeat {
				reply(w, http.StatusOK, nil)
				return
			}
		case <-hold:
			reply(w, http.StatusOK, nil)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// takeJob 刷新心跳并领取一个 queued 任务（heartbeat 不领取）；无任务时返回
// nil 与当前唤醒通道，供长轮询等待下一次入队。
func (b *Bridge) takeJob(heartbeat bool, session string) (map[string]any, <-chan struct{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.touchLocked(session, 0)
	if s := b.sessions[session]; s != nil {
		s.busy = heartbeat
	}
	if !heartbeat && session != "" {
		// 同一面板发来领取轮询 = 它已空闲：交给它的 delivered 任务已丢失（回传被放弃或交付响应丢失）。
		for _, job := range b.jobs {
			if job.State == "delivered" && job.DeliveredTo == session && !b.unresolved[job.ID] {
				b.expireJobLocked(job, "面板已空闲但未回传该任务结果")
			}
		}
	}
	b.expireStaleLocked()
	var deliver *Job
	if !heartbeat {
		for _, job := range b.jobs {
			if job.State == "queued" {
				deliver = job
				break
			}
		}
	}
	if deliver == nil {
		return nil, b.wake, nil
	}
	// 先读脚本再标记交付：读失败时任务留在 queued，可重新领取。
	code, err := os.ReadFile(filepath.Join(b.runDir(deliver.ID), "script.js"))
	if err != nil {
		return nil, nil, fmt.Errorf("脚本读取失败：%w", err)
	}
	now := time.Now()
	deliver.State, deliver.DeliveredTo, deliver.DeliveredAt, deliver.deliveredAt = "delivered", session, nowISO(), now
	if err := writeJSONAtomic(filepath.Join(b.runDir(deliver.ID), "job.json"), *deliver, 0o600); err != nil {
		deliver.State, deliver.DeliveredTo, deliver.DeliveredAt, deliver.deliveredAt = "queued", "", "", time.Time{}
		return nil, nil, fmt.Errorf("任务落盘失败：%w", err)
	}
	return map[string]any{
		"id":           deliver.ID,
		"code":         string(code),
		"targetNodeId": deliver.TargetNodeID,
		"binding":      map[string]string{"fileKey": b.binding.FileKey, "nodeId": b.binding.NodeID},
	}, nil, nil
}

// connectedLocked：有挂起的长轮询，或最近一次轮询在 within 之内。
func (b *Bridge) connectedLocked(within time.Duration) bool {
	return b.waiters > 0 || (!b.lastPoll.IsZero() && time.Since(b.lastPoll) < within)
}

// touchLocked 记录一次轮询活动；delta 为挂起长轮询计数的增减。
func (b *Bridge) touchLocked(session string, delta int) {
	now := time.Now()
	b.lastPoll = now
	b.waiters += delta
	if session == "" {
		return
	}
	s := b.sessions[session]
	if s == nil {
		s = &panelSession{}
		b.sessions[session] = s
	}
	s.lastSeen = now
	s.waiting += delta
}

// expireStaleLocked 自动核销失联/超时的 delivered 任务，并回收静默会话。
// 恢复现场的 unresolved 任务走 inspect+ack 协议，不在此处理。
func (b *Bridge) expireStaleLocked() {
	for _, job := range b.jobs {
		if job.State != "delivered" || b.unresolved[job.ID] {
			continue
		}
		if !job.deliveredAt.IsZero() && time.Since(job.deliveredAt) > execTimeout {
			b.expireJobLocked(job, fmt.Sprintf("交付超过 %.0f 分钟未回传（脚本可能挂起）", execTimeout.Minutes()))
			continue
		}
		var alive bool
		if job.DeliveredTo == "" {
			alive = b.connectedLocked(orphanAfter)
		} else if s := b.sessions[job.DeliveredTo]; s != nil {
			alive = s.waiting > 0 || time.Since(s.lastSeen) < orphanAfter
		}
		if !alive {
			b.expireJobLocked(job, fmt.Sprintf("领取任务的插件面板已断开超过 %.0f 秒", orphanAfter.Seconds()))
		}
	}
	for id, s := range b.sessions {
		if s.waiting == 0 && time.Since(s.lastSeen) > orphanAfter {
			delete(b.sessions, id)
		}
	}
}

// expireJobLocked 把 delivered 任务核销为 expired：先写「结果未知」的失败
// result.json 再改状态；落盘失败则保持 delivered，下次检查重试。迟到的真实
// 回传仍会被接收并覆盖此结果（见 handleComplete）。
func (b *Bridge) expireJobLocked(job *Job, reason string) {
	finished := nowISO()
	result := JobResult{
		ID: job.ID, OK: false, ExecStarted: true, FinishedAt: finished,
		Error: "自动核销：" + reason + "。脚本可能已部分执行，结果未知，普通写入已阻断——先用 inspect:true 只读核实文档现状（成功即自动放行），不要直接重复提交同一修改",
	}
	if err := writeJSONAtomic(filepath.Join(b.runDir(job.ID), "result.json"), result, 0o600); err != nil {
		return
	}
	expired := *job
	expired.State, expired.FinishedAt = "expired", finished
	if err := writeJSONAtomic(filepath.Join(b.runDir(job.ID), "job.json"), expired, 0o600); err != nil {
		return
	}
	*job = expired
	b.unverified[job.ID] = true
}

// busyExpiredLocked 返回仍在其原面板上执行（会话存活且在心跳）的已核销任务：
// 此时面板不领新任务，新提交只会堵在 queued 占住单飞。
func (b *Bridge) busyExpiredLocked() string {
	executing := map[string]bool{}
	for _, job := range b.jobs {
		if job.State == "delivered" {
			executing[job.DeliveredTo] = true
		}
	}
	for _, job := range b.jobs {
		// 该面板已领走更新的任务：它在忙的是那个任务，交给单飞判断。
		if job.State != "expired" || job.DeliveredTo == "" || executing[job.DeliveredTo] {
			continue
		}
		if s := b.sessions[job.DeliveredTo]; s != nil && s.busy && (s.waiting > 0 || time.Since(s.lastSeen) < orphanAfter) {
			return job.ID
		}
	}
	return ""
}

// markVerifiedLocked 解除核销阻断并落盘为 verified（迟到回传走 handleComplete 的终态）。
func (b *Bridge) markVerifiedLocked(id string) {
	delete(b.unverified, id)
	job := b.jobs[id]
	if job == nil || job.State != "expired" {
		return
	}
	verified := *job
	verified.State = "verified"
	if writeJSONAtomic(filepath.Join(b.runDir(id), "job.json"), verified, 0o600) == nil {
		*job = verified
	}
}

func (b *Bridge) expireStale() {
	b.mu.Lock()
	b.expireStaleLocked()
	b.mu.Unlock()
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
		PNGB64      string          `json:"pngB64"`
		PNGsB64     []string        `json:"pngsB64"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil || !jobIDRe.MatchString(msg.ID) {
		reply(w, http.StatusBadRequest, map[string]any{"error": "结果无效"})
		return
	}
	b.mu.Lock()
	job := b.jobs[msg.ID]
	// expired 也接收：迟到的真实结果比核销时的「结果未知」更权威。
	complete := job != nil && (job.State == "delivered" || job.State == "expired")
	if complete {
		// 回传到达即面板存活：刷新心跳，避免旧版面板（无会话 ID）的任务因陈旧
		// lastPoll 被误核销。unresolved 的解除见下方终态锁（result.json 落盘后）。
		b.lastPoll = time.Now()
	}
	b.mu.Unlock()
	if !complete {
		reply(w, http.StatusConflict, map[string]any{"error": "任务不在待回传状态"})
		return
	}
	result := JobResult{
		ID: msg.ID, OK: msg.OK, Output: msg.Output, Before: msg.Before, After: msg.After,
		Error: msg.Error, ExecStarted: msg.ExecStarted, FinishedAt: nowISO(),
	}
	if msg.OK && (msg.PNGB64 != "" || len(msg.PNGsB64) > 0) {
		// 单图 pngB64 与多图 pngsB64 统一落盘：首张 preview.png，后续 preview-N.png，
		// 与插件侧输出占位 {__png:'preview.png'|'preview-N.png'} 命名一致。
		b64s := msg.PNGsB64
		if len(b64s) == 0 {
			b64s = []string{msg.PNGB64}
		}
		if err := os.MkdirAll(b.runDir(job.ID), 0o700); err != nil {
			reply(w, http.StatusInternalServerError, map[string]any{"error": "证据目录创建失败：" + err.Error()})
			return
		}
		names := make([]string, 0, len(b64s))
		for i, s := range b64s {
			png, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				reply(w, http.StatusBadRequest, map[string]any{"error": "图像数据无效"})
				return
			}
			name := "preview.png"
			if i > 0 {
				name = fmt.Sprintf("preview-%d.png", i+1)
			}
			if err := os.WriteFile(filepath.Join(b.runDir(job.ID), name), png, 0o600); err != nil {
				reply(w, http.StatusInternalServerError, map[string]any{"error": "预览图落盘失败：" + err.Error()})
				return
			}
			names = append(names, name)
		}
		result.Preview = strings.Join(names, ",")
	}
	b.mu.Lock()
	// 与自动核销同锁写 result.json，避免两者交错落盘造成结果与状态不一致。
	if job.State != "delivered" && job.State != "expired" {
		b.mu.Unlock()
		reply(w, http.StatusConflict, map[string]any{"error": "任务不在待回传状态"})
		return
	}
	// 先 result 后 job：任一缺失即视为未完成，恢复扫描据此阻断。
	if err := writeJSONAtomic(filepath.Join(b.runDir(job.ID), "result.json"), result, 0o600); err != nil {
		b.mu.Unlock()
		reply(w, http.StatusInternalServerError, map[string]any{"error": "结果落盘失败：" + err.Error()})
		return
	}
	job.State = map[bool]string{true: "done", false: "failed"}[msg.OK]
	job.FinishedAt = result.FinishedAt
	// 迟到回传解除待核实：必须与终态同锁、且在 result.json 落盘成功之后。
	delete(b.unresolved, msg.ID)
	delete(b.unverified, msg.ID)
	if job.Inspect && msg.OK {
		for _, id := range job.verifies {
			b.markVerifiedLocked(id)
		}
	}
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

func (b *Bridge) Submit(code, targetNodeID string, inspectOnly bool) (*Job, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("脚本不能为空")
	}
	if targetNodeID != "" && !targetIDRe.MatchString(targetNodeID) {
		return nil, fmt.Errorf("目标节点 ID 需形如 1:2（收到 %q）", targetNodeID)
	}
	b.mu.Lock()
	b.expireStaleLocked()
	if id := b.busyExpiredLocked(); id != "" {
		b.mu.Unlock()
		return nil, &SubmitError{Msg: fmt.Sprintf(
			"任务 %s 已核销但插件面板仍在执行它（脚本可能挂起），面板此时不领新任务：等待其回传后用 result %s 读回，或在 Figma 中关闭并重开插件面板，约 %.0f 秒后再提交 inspect:true 核实", id, id, orphanAfter.Seconds()),
			Unresolved: []string{id}}
	}
	if len(b.unresolved) > 0 && !inspectOnly {
		ids := make([]string, 0, len(b.unresolved))
		for id := range b.unresolved {
			ids = append(ids, id)
		}
		b.mu.Unlock()
		return nil, &SubmitError{Msg: fmt.Sprintf(
			"上次任务 %s 结果未知，已阻断新写入：请用 figma run（inspect:true）提交只读核实脚本并等待完成，再用 figma ack <job_id> <inspect_job_id> <核实说明> 解锁", strings.Join(ids, ", ")),
			Unresolved: ids}
	}
	if len(b.unverified) > 0 && !inspectOnly {
		ids := make([]string, 0, len(b.unverified))
		for id := range b.unverified {
			ids = append(ids, id)
		}
		b.mu.Unlock()
		return nil, &SubmitError{Msg: fmt.Sprintf(
			"任务 %s 已自动核销、结果未知，已阻断新写入：先用 figma run（inspect:true）只读核实文档现状，核实成功后即自动放行；不要直接重复提交同一修改", strings.Join(ids, ", ")),
			Unresolved: ids}
	}
	for _, job := range b.jobs {
		// 陈旧 delivered（恢复后的 unresolved）不占单飞名额，否则核实入口被自己挡住。
		if (job.State == "queued" || job.State == "delivered") && !b.unresolved[job.ID] {
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
		ID: hex.EncodeToString(idBuf), TargetNodeID: targetNodeID, Inspect: inspectOnly,
		FileKey: b.binding.FileKey, NodeID: b.binding.NodeID,
		State: "queued", SubmittedAt: nowISO(),
	}
	if inspectOnly {
		for id := range b.unverified {
			job.verifies = append(job.verifies, id)
		}
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
	close(b.wake)
	b.wake = make(chan struct{})
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
		b.expireStale()
		if result, err := b.Result(id); err == nil {
			job, _ := b.JobByID(id)
			outcome := WaitOutcome{WaitStatus: "completed", Job: job, Result: result}
			if job.State == "expired" {
				outcome.Instruction = fmt.Sprintf("任务 %s 已自动核销、结果未知：普通写入已阻断，先用 run（inspect:true）只读核实文档现状，成功后自动放行；不要直接重复提交同一修改", id)
			}
			return outcome
		}
		if time.Now().After(deadline) {
			job, _ := b.JobByID(id)
			instr := fmt.Sprintf("任务 %s 未在超时内回传（未被取消）：用 result %s 读回，或稍后再试；不要重复提交同一修改", id, id)
			// 面板断连是超时最常见根因。job.State 为空（磁盘读失败）时不猜断连分支，
			// 落回通用提示，避免拼出「断开（）」。delivered 断连不会重放：handlePoll
			// 只下发 queued；面板 iframe 活着时会自行回传 pendingResult；面板已关则
			// 执行现场没了——领取会话静默超过 orphanAfter 后自动核销为失败（结果未知）。
			if st := b.Status(); !st.Connected && (job.State == "queued" || job.State == "delivered") {
				if job.State == "queued" {
					instr = fmt.Sprintf("插件面板未连接，任务 %s 已入队未被领取：重开插件面板后会自动执行，届时用 result %s 读回；不要重复提交", id, id)
				} else {
					instr = fmt.Sprintf("插件面板在任务 %s 执行期间断开：任务不会被重放——面板仍开着则会自动回传，稍后用 result %s 读回；面板断开超过 %.0f 秒会自动核销为失败（结果未知），届时先用 inspect:true 只读核实文档现状再继续", id, id, orphanAfter.Seconds())
				}
			}
			return WaitOutcome{WaitStatus: "timeout", Job: job, Instruction: instr}
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

// Ack 解除「结果未知」阻断：note 记录核实结论，inspectJobID 必须指向一个
// 已落盘完成的核实任务。两份证据都写成功之后才解除阻断。
func (b *Bridge) Ack(id, inspectJobID, note string) error {
	if strings.TrimSpace(note) == "" {
		return fmt.Errorf("ack 需要附带核实说明（当前文档现状与影响判断）")
	}
	b.mu.Lock()
	if !b.unresolved[id] {
		b.mu.Unlock()
		return fmt.Errorf("任务 %s 不在待核实状态", id)
	}
	if inspectJobID == "" {
		b.mu.Unlock()
		return fmt.Errorf("ack 需要核实任务 ID：先用 figma run（inspect:true）提交只读脚本并等待完成")
	}
	// 核实任务必须是 inspect:true 提交且已完成，且不早于被阻断任务——
	// 否则历史里任意一次旧 run 都能冒充核实，绕过解锁协议。
	inspectJob, jerr := b.JobByID(inspectJobID)
	if jerr != nil || !inspectJob.Inspect {
		b.mu.Unlock()
		return fmt.Errorf("核实任务 %s 无效：需为 figma run（inspect:true）提交的任务", inspectJobID)
	}
	if blocked, ok := b.jobs[id]; ok && inspectJob.SubmittedAt < blocked.SubmittedAt {
		b.mu.Unlock()
		return fmt.Errorf("核实任务 %s 早于被阻断任务，不能作为核实依据：请重新提交 inspect:true", inspectJobID)
	}
	if _, err := b.Result(inspectJobID); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("核实任务 %s 尚无结果：等待其完成后再 ack", inspectJobID)
	}
	b.mu.Unlock()
	// 先写 ack.json（此时 job.json 仍是 delivered）：中途崩溃后恢复扫描会回到
	// unresolved，可重试；两份都成功后才动 job.json 与内存。
	if err := writeJSONAtomic(filepath.Join(b.runDir(id), "ack.json"),
		map[string]any{"id": id, "note": note, "inspectJobId": inspectJobID, "at": nowISO()}, 0o600); err != nil {
		return err
	}
	var jobCopy *Job
	if job, ok := b.jobs[id]; ok {
		jobCopy = new(Job)
		*jobCopy = *job
		jobCopy.State = "acknowledged"
		jobCopy.FinishedAt = nowISO()
	}
	if jobCopy != nil {
		if err := writeJSONAtomic(filepath.Join(b.runDir(id), "job.json"), *jobCopy, 0o600); err != nil {
			return err
		}
	}
	b.mu.Lock()
	delete(b.unresolved, id)
	if job, ok := b.jobs[id]; ok {
		job.State = "acknowledged"
	}
	b.mu.Unlock()
	return nil
}

func (b *Bridge) History() []Job {
	ids, err := b.runEntries()
	if err != nil {
		return nil
	}
	var jobs []Job
	for _, id := range ids {
		if job, err := b.JobByID(id); err == nil {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

func (b *Bridge) Status() StatusReport {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireStaleLocked()
	report := StatusReport{
		Port:         b.port,
		Binding:      &b.binding,
		ManifestPath: b.manifest,
		Connected:    b.connectedLocked(pollFreshSecs * time.Second),
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
		if b.unverified[job.ID] {
			report.Unverified = append(report.Unverified, job.ID)
		}
	}
	return report
}
