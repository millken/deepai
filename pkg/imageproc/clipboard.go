package imageproc

import (
	"bytes"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// ReadClipboardImage attempts to read an image from the system clipboard.
// Returns the raw image bytes and nil error on success.
// Returns nil, nil when the clipboard has no image (caller should fall back
// to normal text-paste behavior).
// Returns nil, error only when a clipboard tool exists but fails unexpectedly.
func ReadClipboardImage() ([]byte, error) {
	switch runtime.GOOS {
	case "darwin":
		return readClipboardDarwin()
	case "linux":
		return readClipboardLinux()
	case "windows":
		return readClipboardWindows()
	default:
		return nil, nil
	}
}

// readClipboardDarwin uses Swift to reliably extract binary image data from
// the clipboard. Swift's AppKit framework provides direct access to
// NSPasteboard image data without the encoding issues of AppleScript hex
// conversion. Falls back to osascript detection first for a quick "is there
// an image?" check.
func readClipboardDarwin() ([]byte, error) {
	// Quick check: is there image data in the clipboard?
	checkCmd := exec.Command("osascript", "-e", "clipboard info")
	if out, err := checkCmd.Output(); err == nil {
		info := string(out)
		if !strings.Contains(info, "PNGf") && !strings.Contains(info, "TIFF") {
			return nil, nil // no image in clipboard
		}
	}

	// Use Swift for reliable binary extraction. AppleScript hex conversion is
	// extremely slow for large images (O(n²) string concatenation) and the
	// «class PNGf» guillemets have encoding issues in some locales.
	cmd := exec.Command("swift", "-e", `
import AppKit
import Foundation

// Try PNG first (most common for screenshots).
if let pngData = NSPasteboard.general.data(forType: .png) {
    FileHandle.standardOutput.write(pngData)
    exit(0)
}
// Try TIFF (some apps copy as TIFF, e.g. Preview).
if let tiffData = NSPasteboard.general.data(forType: .tiff),
   let rep = NSBitmapImageRep(data: tiffData),
   let pngData = rep.representation(using: .png, properties: [:]) {
    FileHandle.standardOutput.write(pngData)
    exit(0)
}
// No image found.
exit(1)
`)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, nil // swift failed or no image → silent fallback
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// readClipboardLinux tries xclip (X11) then wl-paste (Wayland).
func readClipboardLinux() ([]byte, error) {
	// Try xclip first (most common on X11).
	for _, mimeType := range []string{"image/png", "image/jpeg"} {
		cmd := exec.Command("xclip", "-selection", "clipboard", "-t", mimeType, "-o")
		out, err := cmd.Output()
		if err == nil && len(out) > 0 {
			return out, nil
		}
	}

	// Try wl-paste (Wayland).
	for _, mimeType := range []string{"image/png", "image/jpeg"} {
		cmd := exec.Command("wl-paste", "-t", mimeType)
		out, err := cmd.Output()
		if err == nil && len(out) > 0 {
			return out, nil
		}
	}

	return nil, nil // no clipboard tool or no image
}

// readClipboardWindows uses PowerShell to read the clipboard image as PNG.
func readClipboardWindows() ([]byte, error) {
	psScript := `
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$img = [System.Windows.Forms.Clipboard]::GetImage()
if ($img -eq $null) { exit 1 }
$ms = New-Object System.IO.MemoryStream
$img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
[Console]::OpenStandardOutput().Write($ms.ToArray(), 0, $ms.Length)
`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	out, err := cmd.Output()
	if err != nil {
		return nil, nil // no image or PowerShell not available
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ClipboardSupport returns a human-readable description of clipboard image
// support on the current platform, for /doctor diagnostics.
func ClipboardSupport() string {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("osascript"); err == nil {
			return "osascript (native)"
		}
		if _, err := exec.LookPath("swift"); err == nil {
			return "swift (native)"
		}
		return "unavailable (osascript/swift not found)"
	case "linux":
		if _, err := exec.LookPath("xclip"); err == nil {
			return "xclip"
		}
		if _, err := exec.LookPath("wl-paste"); err == nil {
			return "wl-paste (Wayland)"
		}
		return "unavailable (install xclip or wl-paste)"
	case "windows":
		return "PowerShell (native)"
	default:
		return fmt.Sprintf("unsupported platform: %s", runtime.GOOS)
	}
}
