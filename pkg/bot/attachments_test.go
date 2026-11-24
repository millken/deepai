package bot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func makeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 40, G: 120, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadMultipart(t *testing.T, url, token, filename string, content []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAttachmentUploadImageOptimizeAndCleanup(t *testing.T) {
	rt := &recordingRuntime{}
	uploadBase := t.TempDir()
	m, err := NewManager(Options{
		Runtime:       rt,
		Store:         NewMemoryStore(),
		UploadBaseDir: uploadBase,
		IdleTimeout:   -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())

	srv := NewServer(ServerOptions{Manager: m, Token: "secret"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	sess, err := m.CreateSession(CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}

	// Upload a real PNG image + a CSV file.
	pngBytes := makeTestPNG(t, 64, 48)
	rImg := uploadMultipart(t, ts.URL+"/v1/sessions/"+sess.ID+"/attachments", "secret", "../weird name/arch.png", pngBytes)
	if rImg.StatusCode != http.StatusCreated {
		t.Fatalf("upload image status = %d", rImg.StatusCode)
	}
	var imgAtt Attachment
	_ = json.NewDecoder(rImg.Body).Decode(&imgAtt)
	rImg.Body.Close()
	if imgAtt.Kind != "image" || imgAtt.MimeType != "image/png" || !strings.HasPrefix(imgAtt.SandboxPath, "uploads/att_") {
		t.Fatalf("unexpected image attachment: %+v", imgAtt)
	}

	rCSV := uploadMultipart(t, ts.URL+"/v1/sessions/"+sess.ID+"/attachments", "secret", "metrics.csv", []byte("a,b\n1,2\n"))
	if rCSV.StatusCode != http.StatusCreated {
		t.Fatalf("upload csv status = %d", rCSV.StatusCode)
	}
	var csvAtt Attachment
	_ = json.NewDecoder(rCSV.Body).Decode(&csvAtt)
	rCSV.Body.Close()
	if csvAtt.Kind != "file" {
		t.Fatalf("unexpected csv attachment: %+v", csvAtt)
	}

	// Send message referencing both attachments + an inline image.
	turn, err := m.Send(sess.ID, SendRequest{
		Content:       "analyze these",
		AttachmentIDs: []string{imgAtt.ID, csvAtt.ID},
		Images: []models.MessageImage{{
			MimeType: "image/png",
			Base64:   base64.StdEncoding.EncodeToString(pngBytes),
		}},
	})
	if err != nil {
		t.Fatalf("Send with attachments: %v", err)
	}
	waitTurn(t, turn)

	in := rt.lastInput()
	userMsg := in.Messages[len(in.Messages)-1]
	if len(userMsg.Images) != 2 {
		t.Fatalf("userMsg.Images = %d, want 2 (1 inline + 1 uploaded)", len(userMsg.Images))
	}
	for i, im := range userMsg.Images {
		if im.Base64 == "" || im.MimeType == "" {
			t.Fatalf("image[%d] empty: %+v", i, im)
		}
	}
	if !strings.Contains(userMsg.Content, "[Attached file: "+csvAtt.SandboxPath) ||
		!strings.Contains(userMsg.Content, "[Attached image: "+imgAtt.SandboxPath) {
		t.Fatalf("userMsg.Content missing attachment blocks:\n%s", userMsg.Content)
	}
	if got := userMsg.Metadata["attachment_ids"]; got != imgAtt.ID+","+csvAtt.ID {
		t.Fatalf("attachment_ids metadata = %q", got)
	}

	// Image-only send (empty text content) is allowed when attachment_ids is non-empty.
	turn2, err := m.Send(sess.ID, SendRequest{AttachmentIDs: []string{imgAtt.ID}})
	if err != nil {
		t.Fatalf("image-only Send failed: %v", err)
	}
	waitTurn(t, turn2)

	// Record upload dir path before deleting session, then confirm deletion cleans disk!
	sLoaded, err := m.load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sLoaded.mu.Lock()
	sbDir := sLoaded.sandboxDirLocked()
	sLoaded.mu.Unlock()
	if _, err := os.Stat(filepath.Join(sbDir, filepath.FromSlash(imgAtt.SandboxPath))); err != nil {
		t.Fatalf("expected uploaded file on disk before delete: %v", err)
	}

	if err := m.DeleteSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sbDir); !os.IsNotExist(err) {
		t.Fatalf("session upload dir %s was not cleaned up on DeleteSession: err=%v", sbDir, err)
	}
}

func TestAttachmentLimitsAndBotIsolation(t *testing.T) {
	rt := &recordingRuntime{}
	m, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), UploadBaseDir: t.TempDir(), IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())

	srv := NewServer(ServerOptions{Manager: m, Token: "secret"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	sess, _ := m.CreateSession(CreateSessionRequest{})

	// Empty file rejected.
	rEmpty := uploadMultipart(t, ts.URL+"/v1/sessions/"+sess.ID+"/attachments", "secret", "empty.txt", nil)
	ae := decodeAPIError(t, rEmpty)
	rEmpty.Body.Close()
	if rEmpty.StatusCode != http.StatusBadRequest || ae.Code != "invalid_attachment" {
		t.Fatalf("empty upload got (%d, %q)", rEmpty.StatusCode, ae.Code)
	}

	// >10 MiB rejected with 413 attachment_too_large.
	oversized := bytes.Repeat([]byte("a"), int(MaxAttachmentBytes)+16)
	_, err = m.UploadAttachment(sess.ID, "big.bin", bytes.NewReader(oversized))
	if err != ErrAttachmentTooLarge {
		t.Fatalf("oversized upload err = %v, want ErrAttachmentTooLarge", err)
	}

	// Cumulative >50 MiB rejected with ErrUploadQuotaExceeded.
	chunk := bytes.Repeat([]byte("b"), int(MaxAttachmentBytes))
	for i := 0; i < 5; i++ {
		if _, err := m.UploadAttachment(sess.ID, "part.bin", bytes.NewReader(chunk)); err != nil {
			t.Fatalf("upload #%d: %v", i, err)
		}
	}
	if _, err := m.UploadAttachment(sess.ID, "part6.bin", bytes.NewReader([]byte("x"))); err != ErrUploadQuotaExceeded {
		t.Fatalf("quota overflow err = %v, want ErrUploadQuotaExceeded", err)
	}

	// Missing attachment_id returns 404 attachment_not_found.
	rMissing := doReq(t, http.MethodPost, ts.URL+"/v1/sessions/"+sess.ID+"/messages", "secret", map[string]any{
		"content":        "hi",
		"attachment_ids": []string{"att_missing"},
	})
	aeMissing := decodeAPIError(t, rMissing)
	rMissing.Body.Close()
	if rMissing.StatusCode != http.StatusNotFound || aeMissing.Code != "attachment_not_found" {
		t.Fatalf("missing attachment got (%d, %q)", rMissing.StatusCode, aeMissing.Code)
	}

	// Cross-bot isolation: flat /v1/sessions/{reviewerSess}/attachments returns 404.
	_, _ = m.CreateBot(context.Background(), CreateBotRequest{ID: "reviewer", Name: "Reviewer"})
	revSess, _ := m.CreateSessionForBot("reviewer", CreateSessionRequest{})
	rCross := uploadMultipart(t, ts.URL+"/v1/sessions/"+revSess.ID+"/attachments", "secret", "x.txt", []byte("hi"))
	rCross.Body.Close()
	if rCross.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-bot flat upload status = %d, want 404", rCross.StatusCode)
	}
}

func TestFileDownloadSecurityBoundariesAndXSSHeaders(t *testing.T) {
	rt := &recordingRuntime{}
	workDir := t.TempDir()
	m, err := NewManager(Options{
		Runtime:       rt,
		Store:         NewMemoryStore(),
		WorkDir:       workDir,
		UploadBaseDir: t.TempDir(),
		IdleTimeout:   -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())

	srv := NewServer(ServerOptions{Manager: m, Token: "secret"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	sess, _ := m.CreateSession(CreateSessionRequest{})

	// Upload normal text file + active HTML + SVG files.
	txtAtt, err := m.UploadAttachment(sess.ID, "notes.txt", strings.NewReader("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	htmlAtt, err := m.UploadAttachment(sess.ID, "evil.html", strings.NewReader("<script>alert(1)</script>"))
	if err != nil {
		t.Fatal(err)
	}
	svgAtt, err := m.UploadAttachment(sess.ID, "icon.svg", strings.NewReader(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	if err != nil {
		t.Fatal(err)
	}

	// Normal file serves cleanly with nosniff.
	rTxt := doReq(t, http.MethodGet, ts.URL+"/v1/sessions/"+sess.ID+"/files/"+txtAtt.SandboxPath, "secret", nil)
	bodyTxt, _ := io.ReadAll(rTxt.Body)
	rTxt.Body.Close()
	if rTxt.StatusCode != http.StatusOK || string(bodyTxt) != "hello world" {
		t.Fatalf("GET txt = (%d, %q)", rTxt.StatusCode, string(bodyTxt))
	}
	if rTxt.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing nosniff header")
	}

	// HTML & SVG MUST force Content-Disposition: attachment to block same-origin XSS.
	for _, att := range []Attachment{htmlAtt, svgAtt} {
		resp := doReq(t, http.MethodGet, ts.URL+"/v1/sessions/"+sess.ID+"/files/"+att.SandboxPath, "secret", nil)
		disp := resp.Header.Get("Content-Disposition")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(disp, "attachment;") {
			t.Fatalf("active file %s got status=%d Content-Disposition=%q, want attachment", att.Name, resp.StatusCode, disp)
		}
	}

	// Path traversal blocked.
	if _, err := m.ResolveSessionFileForBot(DefaultBotID, sess.ID, "../outside.txt"); err != ErrForbiddenPath {
		t.Fatalf("path traversal err = %v, want ErrForbiddenPath", err)
	}
	if _, err := m.ResolveSessionFileForBot(DefaultBotID, sess.ID, "uploads/../../etc/passwd"); err != ErrForbiddenPath {
		t.Fatalf("nested path traversal err = %v, want ErrForbiddenPath", err)
	}

	// Symlink escape inside sandbox MUST be rejected with ErrForbiddenPath.
	outsideFile := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	sLoaded, _ := m.load(sess.ID)
	sLoaded.mu.Lock()
	sbDir := sLoaded.sandboxDirLocked()
	sLoaded.mu.Unlock()
	symlinkPath := filepath.Join(sbDir, "uploads", "escape.txt")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Fatal(err)
	}
	rEsc := doReq(t, http.MethodGet, ts.URL+"/v1/sessions/"+sess.ID+"/files/uploads/escape.txt", "secret", nil)
	rEsc.Body.Close()
	if rEsc.StatusCode != http.StatusForbidden {
		t.Fatalf("symlink escape HTTP status = %d, want 403", rEsc.StatusCode)
	}
}

func TestAgentRuntimePresentFileArtifactEndToEnd(t *testing.T) {
	workDir := t.TempDir()
	reportPath := filepath.Join(workDir, "report.md")
	if err := os.WriteFile(reportPath, []byte("# Analysis\nAll good."), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideSecret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	evilLink := filepath.Join(workDir, "evil_link.md")
	if err := os.Symlink(outsideSecret, evilLink); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var calls int
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			argsJSON, _ := json.Marshal(map[string]any{"path": reportPath, "description": "Final report"})
			argsEsc, _ := json.Marshal(string(argsJSON))
			chunk := `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_pf","type":"function","function":{"name":"present_file","arguments":` + string(argsEsc) + `}}]},"finish_reason":"tool_calls"}]}`
			_, _ = io.WriteString(w, "data: "+chunk+"\n\ndata: [DONE]\n\n")
			return
		}
		chunk := `{"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Presented report."},"finish_reason":"stop"}]}`
		_, _ = io.WriteString(w, "data: "+chunk+"\n\ndata: [DONE]\n\n")
	}))
	defer mockLLM.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	modelReg, err := llm.NewModelRegistry([]llm.ModelDef{{
		Name:     "mock",
		Provider: "openai-compat",
		Model:    "mock-model",
		BaseURL:  mockLLM.URL,
	}}, "mock")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := NewAgentRuntime(AgentRuntimeConfig{
		Models:         modelReg,
		Tools:          tools.NewRegistry(),
		WorkDir:        workDir,
		SandboxBaseDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), WorkDir: workDir, IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())

	srv := NewServer(ServerOptions{Manager: m, Token: "secret"})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	sess, _ := m.CreateSession(CreateSessionRequest{})
	sub, _, _, _ := m.Subscribe(sess.ID, 0)
	defer sub.Close()

	turn, err := m.Send(sess.ID, SendRequest{Content: "present the report"})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, turn)

	evts := collectUntil(t, sub, EventTurnEnd)
	var art *Artifact
	for _, e := range evts {
		if e.Type == EventArtifact && e.Artifact != nil {
			art = e.Artifact
		}
	}
	if art == nil || art.Name != "report.md" || !strings.HasPrefix(art.DownloadPath, "artifacts/") {
		t.Fatalf("missing or unexpected EventArtifact: %+v (events=%v)", art, eventTypes(evts))
	}

	// Download authorized artifact succeeds.
	rArt := doReq(t, http.MethodGet, ts.URL+"/v1/sessions/"+sess.ID+"/files/"+art.DownloadPath, "secret", nil)
	gotBody, _ := io.ReadAll(rArt.Body)
	rArt.Body.Close()
	if rArt.StatusCode != http.StatusOK || string(gotBody) != "# Analysis\nAll good." {
		t.Fatalf("GET artifact = (%d, %q)", rArt.StatusCode, string(gotBody))
	}

	// SessionInfo.Artifacts includes it.
	info, _, _ := m.GetSession(sess.ID)
	if len(info.Artifacts) != 1 || info.Artifacts[0].ID != art.ID {
		t.Fatalf("SessionInfo.Artifacts = %+v", info.Artifacts)
	}

	// Even if a symlink escaping WorkDir were registered as an artifact, download rejects it.
	sLoaded, _ := m.load(sess.ID)
	sLoaded.mu.Lock()
	sLoaded.artifacts["evil_art"] = Artifact{ID: "evil_art", Name: "evil_link.md", Path: evilLink}
	sLoaded.mu.Unlock()
	rEvil := doReq(t, http.MethodGet, ts.URL+"/v1/sessions/"+sess.ID+"/files/artifacts/evil_art", "secret", nil)
	rEvil.Body.Close()
	if rEvil.StatusCode != http.StatusForbidden {
		t.Fatalf("escaping artifact symlink status = %d, want 403", rEvil.StatusCode)
	}
}
