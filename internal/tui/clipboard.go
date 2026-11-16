package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"zel/internal/core"

	"github.com/atotto/clipboard"
)

func clipboardCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// clipboardPNG writes the pasteboard image as PNG to argv[0] and reports whether there was one.
const clipboardPNG = `ObjC.import("AppKit");
function run(argv) {
 var image = $.NSImage.alloc.initWithPasteboard($.NSPasteboard.generalPasteboard);
 if (image.isNil()) return "text";
 var bitmap = $.NSBitmapImageRep.imageRepWithData(image.TIFFRepresentation);
 if (bitmap.isNil()) throw new Error("Cannot decode clipboard image");
 var png = bitmap.representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $.NSDictionary.dictionary);
 if (png.isNil()) throw new Error("Cannot convert clipboard image to PNG");
 if (!png.writeToFileAtomically($(argv[0]), false)) throw new Error("Cannot save clipboard image");
 return "image";
}`

// readClipboardImage saves a clipboard PNG into directory and returns its path,
// or "" when the clipboard holds no image.
func readClipboardImage(ctx context.Context, directory, platform string, run func(context.Context, string, ...string) ([]byte, error)) (path string, err error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	path = fmt.Sprintf("%s/paste-%d.png", directory, time.Now().UnixNano())
	var data []byte
	switch {
	case platform == "darwin":
		out, err := run(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", clipboardPNG, path)
		if err != nil || strings.TrimSpace(string(out)) != "image" {
			os.Remove(path)
			return "", err
		}
		return path, os.Chmod(path, 0600)
	case os.Getenv("WAYLAND_DISPLAY") != "":
		if types, err := run(ctx, "wl-paste", "--list-types"); err == nil && strings.Contains(string(types), "image/png") {
			data, err = run(ctx, "wl-paste", "--type", "image/png")
			if err != nil {
				return "", err
			}
		}
	default:
		if types, err := run(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"); err == nil && strings.Contains(string(types), "image/png") {
			data, err = run(ctx, "xclip", "-selection", "clipboard", "-t", "image/png", "-o")
			if err != nil {
				return "", err
			}
		}
	}
	if len(data) == 0 {
		return "", nil
	}
	if len(data) > core.MaxImageInputBytes {
		return "", fmt.Errorf("clipboard image exceeds %d bytes", core.MaxImageInputBytes)
	}
	return path, os.WriteFile(path, data, 0600)
}

// readClipboard returns a pasted image path, or the clipboard text when there is no image.
func readClipboard(ctx context.Context, directory string) (string, string, error) {
	path, err := readClipboardImage(ctx, directory, runtime.GOOS, clipboardCommand)
	if err != nil || path != "" {
		return path, "", err
	}
	text, err := clipboard.ReadAll()
	return "", text, err
}
