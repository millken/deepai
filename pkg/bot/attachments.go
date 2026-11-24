package bot

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/millken/deepai/pkg/imageproc"
	"github.com/millken/deepai/pkg/models"
)

const (
	// MaxAttachmentBytes caps one uploaded file at 10 MiB (docs/BOT_NEXT_DESIGN.md §3.1).
	MaxAttachmentBytes int64 = 10 << 20
	// MaxSessionUploadBytes caps total uploads per session at 50 MiB.
	MaxSessionUploadBytes int64 = 50 << 20
)

var (
	ErrAttachmentTooLarge  = errors.New("attachment exceeds 10 MiB limit")
	ErrUploadQuotaExceeded = errors.New("session upload quota (50 MiB) exceeded")
	ErrAttachmentNotFound  = errors.New("attachment not found")
	ErrInvalidAttachment   = errors.New("invalid attachment")
	ErrForbiddenPath       = errors.New("file path is outside allowed session scope")
)

// UploadAttachment uploads a file into a default-bot session sandbox.
func (m *Manager) UploadAttachment(sessionID, filename string, r io.Reader) (Attachment, error) {
	return m.UploadAttachmentForBot(DefaultBotID, sessionID, filename, r)
}

// UploadAttachmentForBot validates, enforces size/quota limits, and stores an
// uploaded file under <sandboxDir>/uploads/<att_id>_<sanitized_name>.
func (m *Manager) UploadAttachmentForBot(botID, sessionID, filename string, r io.Reader) (Attachment, error) {
	botID = effectiveBotID(botID)
	profile, err := m.bots.GetBot(context.Background(), botID)
	if err != nil {
		return Attachment{}, err
	}
	if !profile.Enabled {
		return Attachment{}, fmt.Errorf("%w: %s", ErrBotDisabled, botID)
	}
	s, err := m.loadForBot(botID, sessionID)
	if err != nil {
		return Attachment{}, err
	}
	if r == nil {
		return Attachment{}, fmt.Errorf("%w: empty reader", ErrInvalidAttachment)
	}

	data, err := io.ReadAll(io.LimitReader(r, MaxAttachmentBytes+1))
	if err != nil {
		return Attachment{}, fmt.Errorf("read upload: %w", err)
	}
	if int64(len(data)) > MaxAttachmentBytes {
		return Attachment{}, ErrAttachmentTooLarge
	}
	if len(data) == 0 {
		return Attachment{}, fmt.Errorf("%w: file is empty", ErrInvalidAttachment)
	}

	safeName := sanitizeAttachmentName(filename)
	mimeType := detectAttachmentMIME(safeName, data)
	kind := "file"
	if isSupportedImageMIME(mimeType) {
		kind = "image"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploadBytes+int64(len(data)) > MaxSessionUploadBytes {
		return Attachment{}, ErrUploadQuotaExceeded
	}
	sbDir, err := m.ensureSandboxDirLocked(s)
	if err != nil {
		return Attachment{}, err
	}

	attID := "att_" + randomHex(6)
	relPath := path.Join("uploads", attID+"_"+safeName)
	absPath := filepath.Join(sbDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(absPath), 0o700); err != nil {
		return Attachment{}, fmt.Errorf("create uploads dir: %w", err)
	}
	if err := os.WriteFile(absPath, data, 0o600); err != nil {
		return Attachment{}, fmt.Errorf("write attachment: %w", err)
	}

	att := Attachment{
		ID:          attID,
		Name:        safeName,
		MimeType:    mimeType,
		Size:        int64(len(data)),
		Kind:        kind,
		SandboxPath: relPath,
	}
	if s.attachments == nil {
		s.attachments = map[string]Attachment{}
	}
	s.attachments[attID] = att
	s.uploadBytes += att.Size
	return att, nil
}

// ResolvedFile describes a validated, authorized file ready to stream to HTTP.
type ResolvedFile struct {
	AbsPath       string
	Name          string
	MimeType      string
	ForceDownload bool
}

// ResolveSessionFileForBot strictly resolves relPath against EITHER:
//  1. An explicitly presented artifact ("artifacts/<artifact_id>") registered
//     via present_file in this session (still verified against symlink escape), OR
//  2. A regular file inside this session's sandbox directory (verified via
//     filepath.EvalSymlinks + relUnderRoot).
func (m *Manager) ResolveSessionFileForBot(botID, sessionID, relPath string) (ResolvedFile, error) {
	s, err := m.loadForBot(botID, sessionID)
	if err != nil {
		return ResolvedFile{}, err
	}
	if strings.ContainsRune(relPath, '\x00') {
		return ResolvedFile{}, ErrForbiddenPath
	}
	cleaned := strings.TrimPrefix(relPath, "/")
	if cleaned == "" || strings.Contains(relPath, "\\") {
		return ResolvedFile{}, ErrForbiddenPath
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." {
			return ResolvedFile{}, ErrForbiddenPath
		}
	}

	s.mu.Lock()
	sbDir := s.sandboxDirLocked()
	var art *Artifact
	if strings.HasPrefix(cleaned, "artifacts/") {
		artID := strings.TrimPrefix(cleaned, "artifacts/")
		if a, ok := s.artifacts[artID]; ok {
			cp := a
			art = &cp
		} else if sc, ok := s.conv.(SandboxConversation); ok && sc.PresentFiles() != nil {
			if pf, ok := sc.PresentFiles().Get(artID); ok {
				art = &Artifact{
					ID:          pf.ID,
					Name:        filepath.Base(pf.Path),
					Path:        pf.Path,
					Description: pf.Description,
					MimeType:    pf.MimeType,
					CreatedAt:   pf.CreatedAt,
				}
			}
		}
	}
	s.mu.Unlock()

	if strings.HasPrefix(cleaned, "artifacts/") {
		if art == nil || art.Path == "" {
			return ResolvedFile{}, ErrNotFound
		}
		evalTarget, err := filepath.EvalSymlinks(art.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ResolvedFile{}, ErrNotFound
			}
			return ResolvedFile{}, ErrForbiddenPath
		}
		allowed := false
		if sbDir != "" {
			if evalSB, err := filepath.EvalSymlinks(sbDir); err == nil && isUnderRoot(evalTarget, evalSB) {
				allowed = true
			}
		}
		if !allowed && m.opts.WorkDir != "" {
			if evalWD, err := filepath.EvalSymlinks(m.opts.WorkDir); err == nil && isUnderRoot(evalTarget, evalWD) {
				allowed = true
			}
		}
		if !allowed && m.opts.WorkDir == "" {
			// When WorkDir scoping is disabled (tests), still require exact
			// path match with no symlink escape outside its parent directory.
			allowed = filepath.Clean(evalTarget) == filepath.Clean(art.Path)
		}
		if !allowed {
			return ResolvedFile{}, ErrForbiddenPath
		}
		st, err := os.Stat(evalTarget)
		if err != nil || !st.Mode().IsRegular() {
			return ResolvedFile{}, ErrNotFound
		}
		mt := art.MimeType
		if mt == "" {
			mt = detectFileMIME(evalTarget, art.Name)
		}
		name := sanitizeAttachmentName(art.Name)
		return ResolvedFile{
			AbsPath:       evalTarget,
			Name:          name,
			MimeType:      mt,
			ForceDownload: shouldForceAttachment(name, mt),
		}, nil
	}

	// Sandbox-relative file download.
	if sbDir == "" {
		return ResolvedFile{}, ErrNotFound
	}
	evalSB, err := filepath.EvalSymlinks(sbDir)
	if err != nil {
		return ResolvedFile{}, ErrNotFound
	}
	candidate := filepath.Join(evalSB, filepath.FromSlash(path.Clean("/"+cleaned)))
	if !isUnderRoot(candidate, evalSB) {
		return ResolvedFile{}, ErrForbiddenPath
	}
	evalTarget, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ResolvedFile{}, ErrNotFound
		}
		return ResolvedFile{}, ErrForbiddenPath
	}
	if !isUnderRoot(evalTarget, evalSB) {
		return ResolvedFile{}, ErrForbiddenPath
	}
	st, err := os.Stat(evalTarget)
	if err != nil || !st.Mode().IsRegular() {
		return ResolvedFile{}, ErrNotFound
	}
	name := sanitizeAttachmentName(filepath.Base(evalTarget))
	mt := detectFileMIME(evalTarget, name)
	return ResolvedFile{
		AbsPath:       evalTarget,
		Name:          name,
		MimeType:      mt,
		ForceDownload: shouldForceAttachment(name, mt),
	}, nil
}

// ensureSandboxDirLocked returns the session's sandbox directory, creating the
// conversation (or fallback per-session upload dir) if needed. Caller holds s.mu.
func (m *Manager) ensureSandboxDirLocked(s *session) (string, error) {
	if dir := s.sandboxDirLocked(); dir != "" {
		return dir, nil
	}
	if s.conv == nil {
		conv, err := m.opts.Runtime.NewConversation(s.id)
		if err != nil {
			return "", fmt.Errorf("start conversation: %w", err)
		}
		s.conv = conv
		if dir := s.sandboxDirLocked(); dir != "" {
			return dir, nil
		}
	}
	base := strings.TrimSpace(m.opts.UploadBaseDir)
	if base == "" {
		base = filepath.Join(os.TempDir(), "deepai-bot-uploads")
	}
	dir := filepath.Join(base, "session-"+s.id+"-"+randomHex(4))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create upload dir: %w", err)
	}
	s.ownedUploadDir = dir
	return dir, nil
}

func (s *session) sandboxDirLocked() string {
	if sc, ok := s.conv.(SandboxConversation); ok {
		if d := strings.TrimSpace(sc.SandboxDir()); d != "" {
			return d
		}
	}
	return s.ownedUploadDir
}

// resolveAttachmentsLocked resolves attachment_ids and inline images into
// optimized models.MessageImage entries, appended content references, and
// message metadata. Caller holds s.mu.
func resolveAttachmentsLocked(s *session, content string, attIDs []string, inline []models.MessageImage) (string, []models.MessageImage, map[string]string, error) {
	var images []models.MessageImage
	for _, img := range inline {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(img.Base64))
		if err != nil {
			return "", nil, nil, fmt.Errorf("%w: invalid inline image base64", ErrInvalidAttachment)
		}
		if int64(len(raw)) > MaxAttachmentBytes {
			return "", nil, nil, ErrAttachmentTooLarge
		}
		opt, err := imageproc.Optimize(raw, imageproc.DefaultOptions)
		if err != nil {
			return "", nil, nil, fmt.Errorf("%w: %v", ErrInvalidAttachment, err)
		}
		images = append(images, models.MessageImage{MimeType: opt.MimeType, Base64: opt.Base64})
	}

	if len(attIDs) == 0 {
		return content, images, nil, nil
	}
	sbDir := s.sandboxDirLocked()
	var notes []string
	seen := make(map[string]struct{}, len(attIDs))
	dedupIDs := make([]string, 0, len(attIDs))
	for _, rawID := range attIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		att, ok := s.attachments[id]
		if !ok {
			return "", nil, nil, fmt.Errorf("%w: %s", ErrAttachmentNotFound, id)
		}
		dedupIDs = append(dedupIDs, id)
		if att.Kind == "image" {
			absPath := filepath.Join(sbDir, filepath.FromSlash(att.SandboxPath))
			raw, err := os.ReadFile(absPath)
			if err != nil {
				return "", nil, nil, fmt.Errorf("%w: %v", ErrAttachmentNotFound, err)
			}
			opt, err := imageproc.Optimize(raw, imageproc.DefaultOptions)
			if err != nil {
				return "", nil, nil, fmt.Errorf("%w: decode %s: %v", ErrInvalidAttachment, att.Name, err)
			}
			images = append(images, models.MessageImage{MimeType: opt.MimeType, Base64: opt.Base64})
			notes = append(notes, fmt.Sprintf("[Attached image: %s (%s, %d bytes)]", att.SandboxPath, att.MimeType, att.Size))
		} else {
			notes = append(notes, fmt.Sprintf("[Attached file: %s (%s, %d bytes)]", att.SandboxPath, att.MimeType, att.Size))
		}
	}

	enriched := content
	if len(notes) > 0 {
		block := strings.Join(notes, "\n")
		if enriched != "" {
			enriched = enriched + "\n\n" + block
		} else {
			enriched = block
		}
	}
	var meta map[string]string
	if len(dedupIDs) > 0 {
		meta = map[string]string{"attachment_ids": strings.Join(dedupIDs, ",")}
	}
	return enriched, images, meta, nil
}

func isUnderRoot(target, root string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return false // never serve the root directory itself
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func sanitizeAttachmentName(raw string) string {
	base := filepath.Base(strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/"))
	base = strings.TrimLeft(base, ".")
	var b strings.Builder
	for _, r := range base {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" || out == "." || out == ".." {
		return "file"
	}
	if len(out) > 80 {
		ext := filepath.Ext(out)
		if len(ext) > 12 {
			ext = ""
		}
		out = out[:80-len(ext)] + ext
	}
	return out
}

func detectAttachmentMIME(name string, data []byte) string {
	sniffed := http.DetectContentType(data)
	if idx := strings.Index(sniffed, ";"); idx > 0 {
		sniffed = strings.TrimSpace(sniffed[:idx])
	}
	extMIME := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if idx := strings.Index(extMIME, ";"); idx > 0 {
		extMIME = strings.TrimSpace(extMIME[:idx])
	}
	// Prefer sniffed MIME for images so a spoofed extension can't bypass image decoding.
	if isSupportedImageMIME(sniffed) {
		return sniffed
	}
	if extMIME != "" {
		return extMIME
	}
	if sniffed != "" {
		return sniffed
	}
	return "application/octet-stream"
}

func detectFileMIME(absPath, name string) string {
	f, err := os.Open(absPath)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	var buf [512]byte
	n, _ := f.Read(buf[:])
	return detectAttachmentMIME(name, buf[:n])
}

func isSupportedImageMIME(mt string) bool {
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	}
	return false
}

// shouldForceAttachment forces Content-Disposition: attachment for active or
// scriptable formats so serving session files can never trigger same-origin XSS.
func shouldForceAttachment(name, mimeType string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".html", ".htm", ".svg", ".svgz", ".js", ".mjs", ".cjs", ".xhtml", ".xml", ".swf":
		return true
	}
	mt := strings.ToLower(mimeType)
	for _, needle := range []string{"html", "svg", "javascript", "ecmascript", "xml"} {
		if strings.Contains(mt, needle) {
			return true
		}
	}
	return false
}
