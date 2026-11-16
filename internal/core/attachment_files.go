package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const imageRefPrefix = "zel-image:v1:"

func (s *session) loadPreview(name string) (string, error) {
	base, ext, ok := strings.Cut(strings.TrimPrefix(name, "preview-v1-"), ".")
	digest, err := hex.DecodeString(base)
	if !ok || !strings.HasPrefix(name, "preview-v1-") || len(digest) != sha256.Size || err != nil || (ext != "png" && ext != "jpeg") || strings.ContainsAny(name, "/\\\\") {
		return "", errors.New("invalid attachment reference")
	}
	root, err := os.OpenRoot(filepath.Join(s.sessionDir(), "attachments"))
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxImageInputBytes {
		return "", errors.New("invalid preview file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxImageInputBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > MaxImageInputBytes {
		return "", errors.New("preview exceeds image byte limit")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != base {
		return "", errors.New("preview checksum mismatch")
	}
	return "data:image/" + ext + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// stageInput releases encoded previews before the input enters the queue.
// Legacy embedded images are read as-is; only new inputs are staged.
func (s *session) stageInput(input Input) (Input, error) {
	if len(input.Images) == 0 {
		return input, nil
	}
	input.Images = append(input.Images[:0:0], input.Images...)
	for i, img := range input.Images {
		if strings.HasPrefix(img.URL, imageRefPrefix) {
			continue
		}
		ref, err := s.storePreview(img.URL)
		if err != nil {
			return Input{}, fmt.Errorf("save image preview: %w", err)
		}
		input.Images[i].URL = ref
	}
	return input, nil
}

func (s *session) storePreview(url string) (string, error) {
	header, payload, ok := strings.Cut(url, ",")
	if !ok || (header != "data:image/png;base64" && header != "data:image/jpeg;base64") {
		return "", errors.New("expected a PNG or JPEG data URL")
	}
	if len(payload) > base64.StdEncoding.EncodedLen(MaxImageInputBytes) {
		return "", errors.New("preview exceeds image byte limit")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", err
	}
	format := strings.TrimSuffix(strings.TrimPrefix(header, "data:image/"), ";base64")
	sum := sha256.Sum256(data)
	name := "preview-v1-" + hex.EncodeToString(sum[:]) + "." + format
	dir := filepath.Join(s.sessionDir(), "attachments")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	temp := ".preview-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer root.Remove(temp)
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return "", err
	}
	if err := root.Rename(temp, name); err != nil {
		return "", err
	}
	// Persist the directory entry before history can reference it.
	d, err := root.Open(".")
	if err != nil {
		return "", err
	}
	err = errors.Join(d.Sync(), d.Close())
	if err != nil {
		return "", err
	}
	return imageRefPrefix + name, nil
}

// hydrateImages only runs for the compacted outgoing request. Display and
// persistence never need to read file-backed preview bytes.
func (s *session) hydrateImages(input []json.RawMessage) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for i, item := range input {
		if !strings.Contains(string(item), imageRefPrefix) {
			continue
		}
		var v map[string]json.RawMessage
		if err := json.Unmarshal(item, &v); err != nil {
			return nil, err
		}
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(v["content"], &parts); err != nil {
			return nil, err
		}
		changed := false
		for _, part := range parts {
			var url string
			if json.Unmarshal(part["image_url"], &url) != nil || !strings.HasPrefix(url, imageRefPrefix) {
				continue
			}
			hydrated, err := s.loadPreview(strings.TrimPrefix(url, imageRefPrefix))
			if err != nil {
				return nil, fmt.Errorf("image preview unavailable (%s): %w", url, err)
			}
			part["image_url"] = raw(hydrated)
			changed = true
		}
		if changed {
			if out == nil {
				out = append([]json.RawMessage(nil), input...)
			}
			v["content"] = raw(parts)
			out[i] = raw(v)
		}
	}
	if out != nil {
		return out, nil
	}
	return input, nil
}
