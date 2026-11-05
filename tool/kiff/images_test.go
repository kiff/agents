package kiff_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiff/agents/tool/kiff"
)

// encoded is a w×h image, as a PNG or a JPEG file.
func encoded(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var err error
	if format == "png" {
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A tool's images (a screenshot of the app Coach drives, kiff/tafs#544) come
// with its text for the model, each kept only once it decodes as the type it
// says, within the bounds: one the model couldn't read would fail every later
// turn that carries it (qa-kiff on #339). Anything else is a line saying so,
// never dropped silently. The recorded form keeps the text, and a line saying
// the images were there.
func TestAToolsImagesComeWithItsText(t *testing.T) {
	shot := encoded(t, "png", 540, 1100)
	photo := encoded(t, "jpeg", 8, 8)
	b64 := base64.StdEncoding.EncodeToString
	big := make([]byte, kiff.MaxImageBytes+1)
	content := []any{
		map[string]any{"type": "text", "text": "Tapped Run now"},
		map[string]any{"type": "image", "data": b64(shot), "mimeType": "image/png"},
		map[string]any{"type": "image", "data": "not base64!", "mimeType": "image/png"},                              // not base64
		map[string]any{"type": "image", "data": b64(shot), "mimeType": "image/webp"},                                 // a type it can't check
		map[string]any{"type": "image", "data": b64(shot[:len(shot)/2]), "mimeType": "image/png"},                    // cut short
		map[string]any{"type": "image", "data": b64(shot), "mimeType": "image/jpeg"},                                 // a PNG sent as a JPEG
		map[string]any{"type": "image", "data": b64([]byte("\x89PNG\r\n\x1a\nnot really")), "mimeType": "image/png"}, // a header and no image
		map[string]any{"type": "image", "data": b64(encoded(t, "png", kiff.MaxImageSide+1, 1)), "mimeType": "image/png"},
		map[string]any{"type": "image", "data": b64(big), "mimeType": "image/jpeg"},
	}
	for range kiff.MaxImages {
		content = append(content, map[string]any{"type": "image", "data": b64(photo), "mimeType": "image/jpeg"})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s")
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "app_look", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}}}}
		case "tools/call":
			result = map[string]any{"content": content}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	defer srv.Close()
	tools, err := kiff.New(srv.URL, testKey, srv.Client()).Tools(context.Background())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	got, err := tools[0].Call(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Images) != kiff.MaxImages || got.Images[0].MediaType != "image/png" || !bytes.Equal(got.Images[0].Bytes, shot) || got.Images[1].MediaType != "image/jpeg" {
		kept := make([]string, len(got.Images))
		for i, img := range got.Images {
			kept[i] = fmt.Sprintf("%s (%d bytes)", img.MediaType, len(img.Bytes))
		}
		t.Fatalf("images kept: %v", kept)
	}
	for _, line := range []string{"Tapped Run now", "[an image the tool sent that couldn't be read]", "an image of 3001×1 pixels, over the 3000 a side",
		"over the 1048576 a tool's image may be", "[more than 4 images: the rest left out]", "[4 image(s) with this answer, for this run only: not kept once it pauses]"} {
		if !strings.Contains(got.Text, line) {
			t.Errorf("text %q is missing %q", got.Text, line)
		}
	}
	// Not base64, an unchecked type, cut short, a PNG sent as a JPEG, a header alone: each says so.
	if n := strings.Count(got.Text, "couldn't be read"); n != 5 {
		t.Errorf("%d bad images said so, want 5: %q", n, got.Text)
	}
	// The recorded form never carries them, only the line that they were there.
	rec, _ := json.Marshal(got)
	if strings.Contains(string(rec), b64(shot)) || strings.Contains(string(rec), `"images"`) || strings.Contains(string(rec), `"Images"`) {
		t.Fatalf("recorded: %.300s", rec)
	}
}
