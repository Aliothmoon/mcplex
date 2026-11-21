package capabilities

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aliothmoon/mcplex/pkg/policy"
)

func TestDirectReadTextLineRangeAndTail(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, "line-"+string(rune('A'+(i%26)))+"-content")
	}
	p := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})

	res, err := ReadFileDirect(pol, dir, DirectReadRequest{
		Path:      "sample.txt",
		StartLine: 10,
		EndLine:   15,
	})
	if err != nil {
		t.Fatalf("ReadFileDirect range failed: %v", err)
	}
	if res.IsImage {
		t.Fatal("expected text result")
	}
	if !strings.Contains(res.Text, "L10-15 of 50") || !strings.Contains(res.Text, "10: ") || !strings.Contains(res.Text, "15: ") {
		t.Fatalf("unexpected range output:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "startLine: 16") {
		t.Fatalf("expected pagination hint startLine: 16, got:\n%s", res.Text)
	}

	tailRes, err := ReadFileDirect(pol, dir, DirectReadRequest{
		Path: "sample.txt",
		Tail: 5,
	})
	if err != nil {
		t.Fatalf("ReadFileDirect tail failed: %v", err)
	}
	if !strings.Contains(tailRes.Text, "tail 5 lines") || !strings.Contains(tailRes.Text, "50: ") {
		t.Fatalf("unexpected tail output:\n%s", tailRes.Text)
	}
}

func TestDirectReadByteOffsetPagination(t *testing.T) {
	dir := t.TempDir()
	longLine := strings.Repeat("abcdefghij", 400)
	if err := os.WriteFile(filepath.Join(dir, "min.js"), []byte(longLine), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})

	noNums := false
	r1, err := ReadFileDirect(pol, dir, DirectReadRequest{
		Path:        "min.js",
		LineNumbers: &noNums,
		MaxBytes:    1000,
	})
	if err != nil {
		t.Fatalf("chunk 1 failed: %v", err)
	}
	if !strings.Contains(r1.Text, "byteOffset: 1000") {
		t.Fatalf("expected byteOffset continuation hint, got:\n%s", r1.Text)
	}

	r2, err := ReadFileDirect(pol, dir, DirectReadRequest{
		Path:        "min.js",
		LineNumbers: &noNums,
		ByteOffset:  1000,
		MaxBytes:    1000,
	})
	if err != nil {
		t.Fatalf("chunk 2 failed: %v", err)
	}
	if !strings.Contains(r2.Text, "byteOffset: 2000") {
		t.Fatalf("expected byteOffset 2000 hint, got:\n%s", r2.Text)
	}
}

func TestDirectReadPNGImageMultimodal(t *testing.T) {
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})

	res, err := ReadFileDirect(pol, dir, DirectReadRequest{Path: "shot.png"})
	if err != nil {
		t.Fatalf("ReadFileDirect PNG failed: %v", err)
	}
	if !res.IsImage || res.ImageMIME != "image/png" {
		t.Fatalf("expected PNG multimodal result, got isImage=%v mime=%q", res.IsImage, res.ImageMIME)
	}
	if !strings.Contains(res.Text, "4x3") {
		t.Fatalf("expected image dimensions 4x3 in caption, got: %s", res.Text)
	}
	decoded, err := base64.StdEncoding.DecodeString(res.ImageData)
	if err != nil || !bytes.Equal(decoded, buf.Bytes()) {
		t.Fatalf("decoded image bytes mismatch: err=%v", err)
	}
}

func TestDirectReadRejectsBinaryAndEnforcesPolicy(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "bin.dat"), []byte{0x00, 0x01, 0x02, 0xff}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "secret.env"), []byte("API_KEY=secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	pol.LoadSettingsJSON([]byte(`{"permissions":{"deny":["read_file(secret.env)"]}}`))

	if _, err := ReadFileDirect(pol, ws, DirectReadRequest{Path: "bin.dat"}); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("expected binary error, got: %v", err)
	}
	if _, err := ReadFileDirect(pol, ws, DirectReadRequest{Path: "secret.env"}); err == nil {
		t.Fatal("expected policy deny rule to block reading secret.env")
	}
}
