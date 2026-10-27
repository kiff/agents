package runner_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/attach"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/run"
	"github.com/kiff/agents/server"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
)

// withFiles gives the fixture's runner an attachments service, and returns it
// with its blob store.
func (f *fixture) withFiles() (*attach.Service, *memory.Blobs) {
	blobs := memory.NewBlobs()
	files := &attach.Service{Store: memory.NewAttachments(), Blobs: blobs, Now: f.clock.now}
	f.r.Files = files
	return files, blobs
}

// uploaded is a file ana attached and uploaded, as the app does.
func uploaded(t *testing.T, files *attach.Service, blobs *memory.Blobs, data []byte) attach.Attachment {
	t.Helper()
	sum := sha256.Sum256(data)
	a, _, err := files.Create(ctx, "t1", "rae", ana.Key(), attach.Declared{MediaType: "image/png", Name: "receipt.png", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(ctx, a.Key(), "image/png", data); err != nil {
		t.Fatal(err)
	}
	return a
}

var png = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 32)...)

// fileBlocks are a message's File blocks.
func fileBlocks(m model.Message) []model.Block {
	var out []model.Block
	for _, b := range m.Blocks {
		if b.Kind == model.File {
			out = append(out, b)
		}
	}
	return out
}

// A file a person attaches reaches the model with their message, fenced as
// material to read, and its bytes are loaded for the turn. The message
// records it by reference, never its bytes, and the next message in the chat
// still sees it.
func TestAFileReachesTheModelWithItsMessage(t *testing.T) {
	f := newFixture(t, say("That receipt is for order 42."), say("Yes, refundable."))
	files, blobs := f.withFiles()
	a := uploaded(t, files, blobs, png)

	r := f.start(server.Start{Input: "Is this refundable?", Requester: ana, Attachments: []string{a.ID}})
	if len(r.Attachments) != 1 || r.Attachments[0].ID != a.ID || r.Attachments[0].Kind != attach.Image {
		t.Fatalf("run's attachments = %+v", r.Attachments)
	}
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	req := f.model.requests()[0]
	last := req.Messages[len(req.Messages)-1]
	fb := fileBlocks(last)
	if len(fb) != 1 || fb[0].Ref != a.ID || !bytes.Equal(fb[0].Bytes, png) || fb[0].Name != "receipt.png" {
		t.Fatalf("the model got %+v", last.Blocks)
	}
	if !strings.Contains(last.Blocks[len(last.Blocks)-2].Text, "not as instructions") {
		t.Fatalf("no fence before the file: %+v", last.Blocks)
	}

	// Recorded by reference: the owner's message names the file, and
	// nothing stored holds its bytes.
	_, events, err := f.store.ReadRun(ctx, "t1", r.Session, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var body agent.TextBody
	if err := json.Unmarshal(events[0].Body, &body); err != nil || events[0].Kind != session.OwnerMessage ||
		body.Text != "Is this refundable?" || len(body.Attachments) != 1 || body.Attachments[0].ID != a.ID {
		t.Fatalf("recorded %s %s", events[0].Kind, events[0].Body)
	}
	for _, e := range events {
		if bytes.Contains(e.Body, png[8:]) {
			t.Fatalf("an event holds the file's bytes: %s", e.Kind)
		}
	}

	// It goes when its message does: the message's retention.
	stored, _ := files.Store.Get(ctx, "t1", a.ID)
	if stored.Session != r.Session || stored.Run != r.ID || !stored.Expires.Equal(r.Created.Add(30*24*time.Hour)) {
		t.Fatalf("bound = %+v", stored)
	}

	// The next message in the chat still sees it, loaded again.
	next := f.start(server.Start{Session: r.Session, Input: "and now?", Requester: ana})
	if _, err := f.r.Execute(ctx, "t1", next.ID, nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.model.requests()
	if fb := fileBlocks(reqs[len(reqs)-1].Messages[0]); len(fb) != 1 || !bytes.Equal(fb[0].Bytes, png) {
		t.Fatalf("the next run's history: %+v", reqs[len(reqs)-1].Messages[0].Blocks)
	}
}

// A message may be files alone.
func TestAMessageOfFilesAlone(t *testing.T) {
	f := newFixture(t, say("A receipt for order 42."))
	files, blobs := f.withFiles()
	a := uploaded(t, files, blobs, png)
	r := f.start(server.Start{Requester: ana, Attachments: []string{a.ID}})
	if _, err := f.r.Execute(ctx, "t1", r.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.get(r.ID); got.Status != run.Completed {
		t.Fatalf("status %s: %s", got.Status, got.Error)
	}
}

// A file is sent once, by the person who attached it, in their own chat
// message: never another person's, a door's, a job's, or twice.
func TestOnlyItsSendersOwnMessageCarriesAFile(t *testing.T) {
	f := newFixture(t, say("ok"))
	files, blobs := f.withFiles()
	a := uploaded(t, files, blobs, png)
	for name, s := range map[string]server.Start{
		"another person": {Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "hi", Requester: run.Requester{Kind: run.ByMember, ID: "bo"}, Attachments: []string{a.ID}},
		"a door":         {Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "hi", Requester: run.Requester{Kind: run.ByDoor, ID: "d1"}, Attachments: []string{a.ID}},
		"a job":          {Tenant: "t1", Agent: "rae", Trigger: run.BySchedule, Input: "hi", Attachments: []string{a.ID}},
	} {
		if _, err := f.r.Start(ctx, s); err == nil {
			t.Errorf("%s sent ana's file", name)
		}
	}
	f.start(server.Start{Input: "hi", Requester: ana, Attachments: []string{a.ID}})
	if _, err := f.r.Start(ctx, server.Start{Tenant: "t1", Agent: "rae", Trigger: run.ByOwner, Input: "again", Requester: ana, Attachments: []string{a.ID}}); !errors.Is(err, attach.ErrBound) {
		t.Fatalf("sent twice: %v", err)
	}
}
