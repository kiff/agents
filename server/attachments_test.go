package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/attach"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
)

// withFiles serves the API with an attachments service in memory.
func (f *runFixture) withFiles() (*attach.Service, *memory.Blobs) {
	blobs := memory.NewBlobs()
	files := &attach.Service{Store: memory.NewAttachments(), Blobs: blobs, Now: func() time.Time { return now }}
	f.server.Files = files
	f.restart()
	return files, blobs
}

func pngDeclared() (map[string]any, []byte) {
	data := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 24)...)
	sum := sha256.Sum256(data)
	return map[string]any{"media_type": "image/png", "name": "receipt.png", "size": len(data), "sha256": hex.EncodeToString(sum[:])}, data
}

// A person declares a file and gets an upload for exactly it; a file is
// refused before any upload when it can't be attached, saying why; and only
// a person attaches one.
func TestAttachingAFile(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.withFiles()
	decl, _ := pngDeclared()

	code, body := f.do("t1", "POST", "/agents/payouts/attachments", decl)
	f.want(400, code, body, "no person")
	code, body = f.do("t1", "POST", "/agents/payouts/attachments?as=service", decl)
	f.want(400, code, body, "as the service")
	code, body = f.do("t1", "POST", "/agents/payouts/attachments?owner=door:d1", decl)
	f.want(400, code, body, "a door")
	code, body = f.do("t1", "POST", "/agents/nobody/attachments?owner=member:ana", decl)
	f.want(404, code, body, "no such agent")

	code, body = f.do("t1", "POST", "/agents/payouts/attachments?owner=member:ana",
		map[string]any{"media_type": "image/jpeg", "name": "huge.jpg", "size": 11 << 20, "sha256": decl["sha256"]})
	f.want(400, code, body, "a photo over 10 MB")
	if msg, _ := body["message"].(string); body["error"] != "invalid" || !strings.Contains(msg, "huge.jpg is 11.0 MB; the most is 10.0 MB") {
		t.Fatalf("the limit isn't said: %q", msg)
	}
	code, body = f.do("t1", "POST", "/agents/payouts/attachments?owner=member:ana",
		map[string]any{"media_type": "application/x-sh", "name": "run.sh", "size": 10, "sha256": decl["sha256"]})
	f.want(400, code, body, "a script")

	code, body = f.do("t1", "POST", "/agents/payouts/attachments?owner=member:ana", decl)
	f.want(201, code, body, "ana attaches a photo")
	att := body["attachment"].(map[string]any)
	up := body["upload"].(map[string]any)
	if att["kind"] != "image" || att["sent"] != false || up["method"] != "PUT" || !strings.HasPrefix(att["id"].(string), "att_") {
		t.Fatalf("created %v", body)
	}
}

// Only the person who attached a file reads it: not another person, and not
// the service, an admin or the account's owner.
func TestOnlyItsSenderReadsAFile(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.withFiles()
	decl, _ := pngDeclared()
	_, body := f.do("t1", "POST", "/agents/payouts/attachments?owner=member:ana", decl)
	id := body["attachment"].(map[string]any)["id"].(string)

	code, body := f.do("t1", "GET", "/agents/payouts/attachments/"+id+"?owner=member:ana", nil)
	f.want(200, code, body, "ana reads hers")
	if u, _ := body["url"].(string); u == "" || body["name"] != "receipt.png" {
		t.Fatalf("read %v", body)
	}
	code, body = f.do("t1", "GET", "/agents/payouts/attachments/"+id+"?owner=member:bo", nil)
	f.want(404, code, body, "bo reads ana's")
	code, body = f.do("t1", "GET", "/agents/payouts/attachments/"+id+"?as=service", nil)
	f.want(400, code, body, "the service reads ana's")
	code, body = f.do("t2", "GET", "/agents/payouts/attachments/"+id+"?owner=member:ana", nil)
	f.want(404, code, body, "another tenant")
}

// A door's message never carries a file.
func TestADoorSendsNoFile(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	f.withFiles()
	code, body := f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "hi", "background": true,
		"requester": map[string]any{"kind": "door", "id": "d1"}, "attachments": []string{"att_1"}})
	f.want(400, code, body, "a door's message with a file")
	code, body = f.do("t1", "POST", "/agents/payouts/runs", map[string]any{"input": "", "background": true,
		"requester": map[string]any{"kind": "member", "id": "ana"}})
	f.want(400, code, body, "an empty message with no file")
}

// Deleting a chat deletes its files; removing the agent, every file it had.
func TestFilesGoWithTheirChat(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "payouts")
	files, blobs := f.withFiles()
	decl, data := pngDeclared()
	attachOne := func() attach.Attachment {
		_, body := f.do("t1", "POST", "/agents/payouts/attachments?owner=member:ana", decl)
		a, err := files.Store.Get(ctx, "t1", body["attachment"].(map[string]any)["id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		_ = blobs.Put(ctx, a.Key(), "image/png", data)
		return a
	}
	rec, err := session.NewRecorder(f.store, session.DefaultPolicy(0))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := rec.OpenOwned(ctx, "t1", "payouts", session.KindChat, "member:ana")
	if err != nil {
		t.Fatal(err)
	}
	sent := attachOne()
	as, err := files.Take(ctx, "t1", "payouts", "member:ana", []string{sent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Bind(ctx, as, chat.ID, "r1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	unsent := attachOne()

	code, body := f.do("t1", "DELETE", "/sessions/"+chat.ID+"?owner=member:ana", nil)
	f.want(204, code, body, "ana deletes her chat")
	if _, err := files.Store.Get(ctx, "t1", sent.ID); err == nil {
		t.Fatal("the chat's file outlived it")
	}
	if keys := blobs.Keys(); len(keys) != 1 || keys[0] != unsent.Key() {
		t.Fatalf("left %v, want only the unsent upload", keys)
	}

	code, body = f.do("t1", "DELETE", "/agents/payouts", nil)
	f.want(204, code, body, "remove the agent")
	if keys := blobs.Keys(); len(keys) != 0 {
		t.Fatalf("left %v after the agent went", keys)
	}
}
