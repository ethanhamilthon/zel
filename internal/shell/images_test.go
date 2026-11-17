package shell

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImagePreviewsResize(t *testing.T) {
	for _, tc := range []struct {
		name, format                         string
		width, height, wantWidth, wantHeight int
	}{
		{"wide-sheet-regression", "jpeg", 3290, 1170, 2048, 728},
		{"portrait", "png", 1170, 3290, 728, 2048},
		{"small", "png", 25, 15, 25, 15},
		{"thin", "png", 1, 4000, 1, 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := image.NewNRGBA(image.Rect(0, 0, tc.width, tc.height))
			src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 100})
			path, original := writeImage(t, src, tc.format)
			captured, err := CaptureImage(path)
			if err != nil {
				t.Fatal(err)
			}
			preview, _, data := decodePreview(t, captured)
			if preview.Bounds().Dx() != tc.wantWidth || preview.Bounds().Dy() != tc.wantHeight {
				t.Fatalf("wrong dimensions: %v", preview.Bounds())
			}
			if tc.format == "png" {
				_, _, _, alpha := preview.At(preview.Bounds().Max.X-1, preview.Bounds().Max.Y-1).RGBA()
				if alpha != 0 {
					t.Fatal("lost transparency")
				}
			}
			if tc.name == "small" && !bytes.Equal(data, original) {
				t.Fatal("unnecessarily re-encoded small image")
			}
			if captured.Path != path {
				t.Fatalf("path = %q, want the file itself, %q", captured.Path, path)
			}
			if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
				t.Fatal("capturing must not write files")
			}
		})
	}
}

func TestViewRejectsInvalidAndOversizedImages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.png")
	_, pngData := writeImage(t, image.NewNRGBA(image.Rect(0, 0, 2, 2)), "png")
	oversized := bytes.Clone(pngData)
	binary.BigEndian.PutUint32(oversized[16:20], 5000)
	binary.BigEndian.PutUint32(oversized[20:24], 5000)
	binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
	for _, data := range [][]byte{[]byte("not an image"), pngData[:33], oversized} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := CaptureImage(path); err == nil {
			t.Fatal("accepted invalid/truncated/oversized image")
		}
	}
	if err := os.Truncate(path, MaxImageInputBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureImage(path); err == nil || !strings.Contains(err.Error(), "32 MiB") {
		t.Fatal("accepted oversized input", err)
	}
	if _, err := CaptureImage(dir); err == nil {
		t.Fatal("accepted directory")
	}
}
