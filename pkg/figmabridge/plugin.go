// Package figmabridge 在 deepai 进程内运行本地 Figma 桥：插件轮询领取 JS
// 任务、在 (figma, target) 作用域执行、结果与快照落盘为证据。协议与磁盘
// 格式为本项目自有设计（借鉴 local-figma 的架构思想，未复用其代码）。
package figmabridge

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed assets/main.js assets/ui.html
var assets embed.FS

type Binding struct {
	ID        string `json:"id"`
	SourceURL string `json:"sourceUrl"`
	FileKey   string `json:"fileKey"`
	NodeID    string `json:"nodeId"`
	BoundAt   string `json:"boundAt"`
}

func StateDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法定位用户配置目录: %w", err)
	}
	dir := filepath.Join(base, "deepai", "figma")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("无法创建状态目录: %w", err)
	}
	return dir, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSONAtomic(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadBinding(dir string) (Binding, error) {
	var b Binding
	if err := readJSON(filepath.Join(dir, "binding.json"), &b); err != nil {
		return b, fmt.Errorf("尚未绑定 Figma 目标，请先用 figma init <画板链接>")
	}
	return b, nil
}

var (
	figmaURLPath = regexp.MustCompile(`^/(?:design|file)/([A-Za-z0-9_-]+)(?:/|$)`)
	nodeIDRe     = regexp.MustCompile(`^\d+:\d+$`)
)

func ParseFigmaURL(raw string) (Binding, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" ||
		(u.Hostname() != "figma.com" && u.Hostname() != "www.figma.com") {
		return Binding{}, fmt.Errorf("需要 figma.com 的 HTTPS 链接")
	}
	m := figmaURLPath.FindStringSubmatch(u.Path)
	nodeID := strings.ReplaceAll(u.Query().Get("node-id"), "-", ":")
	if m == nil || !nodeIDRe.MatchString(nodeID) {
		return Binding{}, fmt.Errorf("请提供包含 node-id 的画板链接（右键画板 → Copy link to selection）")
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Binding{}, err
	}
	return Binding{
		ID:        hex.EncodeToString(id),
		SourceURL: raw,
		FileKey:   m[1],
		NodeID:    nodeID,
		BoundAt:   nowISO(),
	}, nil
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// PairingToken 首次生成 64hex 并持久化，deepai 重启后复用——插件导入一次即可。
func PairingToken(dir string) (string, error) {
	path := filepath.Join(dir, "pairing.json")
	var v struct {
		Token string `json:"token"`
	}
	if err := readJSON(path, &v); err == nil {
		if len(v.Token) == 64 {
			return v.Token, nil
		}
		return "", fmt.Errorf("配对资料损坏，请删除 %s 后重新 init", path)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	v.Token = hex.EncodeToString(buf)
	if err := writeJSONAtomic(path, v, 0o600); err != nil {
		return "", err
	}
	return v.Token, nil
}

// GeneratePlugin 在状态目录生成可导入的开发插件，返回 manifest 路径。
func GeneratePlugin(dir string, port int, token string, b Binding) (string, error) {
	pdir := filepath.Join(dir, "plugin")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		return "", err
	}
	mainJS, err := assets.ReadFile("assets/main.js")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(pdir, "main.js"), mainJS, 0o600); err != nil {
		return "", err
	}
	uiHTML, err := assets.ReadFile("assets/ui.html")
	if err != nil {
		return "", err
	}
	config, err := json.Marshal(map[string]any{
		"port":    port,
		"token":   token,
		"binding": map[string]string{"fileKey": b.FileKey, "nodeId": b.NodeID},
	})
	if err != nil {
		return "", err
	}
	// < 转义防止序列化配置提前闭合 script 标签。
	injected := strings.ReplaceAll(string(config), "<", "\\u003c")
	ui := strings.Replace(string(uiHTML), "SESSION_CONFIG", injected, 1)
	if err := os.WriteFile(filepath.Join(pdir, "ui.html"), []byte(ui), 0o600); err != nil {
		return "", err
	}
	manifest := map[string]any{
		"name":           "deepai Figma Bridge",
		"api":            "1.0.0",
		"main":           "main.js",
		"ui":             "ui.html",
		"editorType":     []string{"figma"},
		"documentAccess": "dynamic-page",
		"networkAccess": map[string]any{
			"allowedDomains":    []string{"none"},
			"devAllowedDomains": []string{fmt.Sprintf("http://127.0.0.1:%d", port)},
		},
	}
	mpath := filepath.Join(pdir, "manifest.json")
	if err := writeJSONAtomic(mpath, manifest, 0o600); err != nil {
		return "", err
	}
	return mpath, nil
}
