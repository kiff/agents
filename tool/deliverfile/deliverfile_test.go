package deliverfile_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kiff/agents/files"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool"
	"github.com/kiff/agents/tool/deliverfile"
)

var ctx = context.Background()

func newTool() (*deliverfile.Tool, *files.Service) {
	svc := &files.Service{Store: memory.NewFiles(), Blobs: memory.NewBlobs()}
	return deliverfile.New(svc, "t1", "refunds", "r1", func(_ context.Context, tenant, run string) (string, error) {
		return "chat-of-" + run, nil
	}), svc
}

func input(name string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"name": name, "media_type": "text/csv", "content": "a,b\n1,2\n"})
	return b
}

// deliver_file is the agent's own tool: no gateway, and no Card governs it,
// so it's never counted with the calls that act outside the account.
func TestItIsTheAgentsOwnTool(t *testing.T) {
	d, _ := newTool()
	if d.Access() != tool.Own || d.Gateway() {
		t.Errorf("access %v, gateway %v: want Own, no gateway", d.Access(), d.Gateway())
	}
	if tool.Reads(d, input("x")) {
		t.Error("a delivery counts as a read")
	}
}

// A delivery is kept in the run's chat, and the result names it.
func TestADeliveryIsKeptInTheRunsChat(t *testing.T) {
	d, svc := newTool()
	r, err := d.Call(ctx, input("week"))
	if err != nil || r.IsError || !strings.Contains(r.Text, `"week.csv"`) {
		t.Fatalf("%+v, %v", r, err)
	}
	all, _ := svc.List(ctx, "t1", files.Filter{Session: "chat-of-r1"})
	if len(all) != 1 || all[0].Agent != "refunds" || all[0].Run != "r1" {
		t.Errorf("kept: %+v", all)
	}
}

// What can't be kept is an error result the model reads, saying why.
func TestARefusalIsAResultSayingWhy(t *testing.T) {
	d, _ := newTool()
	for _, in := range []string{`{`, `{"name":"x","media_type":"text/html","content":"<b>"}`, `{"name":"x","media_type":"text/plain","content":""}`} {
		r, err := d.Call(ctx, json.RawMessage(in))
		if err != nil || !r.IsError {
			t.Errorf("%s: %+v, %v", in, r, err)
		}
	}
}

// Ten calls at once deliver five, and the other five are told why.
func TestCallsAtOnceDeliverAtMostFive(t *testing.T) {
	d, svc := newTool()
	var wg sync.WaitGroup
	var mu sync.Mutex
	refused := 0
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := d.Call(ctx, input(fmt.Sprintf("f%d", i)))
			if err != nil {
				t.Error(err)
			}
			if r.IsError {
				mu.Lock()
				refused++
				mu.Unlock()
				if !strings.Contains(r.Text, "at most 5") {
					t.Errorf("refusal: %q", r.Text)
				}
			}
		}()
	}
	wg.Wait()
	all, _ := svc.List(ctx, "t1", files.Filter{Run: "r1"})
	if len(all) != files.MaxPerRun || refused != 10-files.MaxPerRun {
		t.Errorf("%d kept and %d refused, want %d and %d", len(all), refused, files.MaxPerRun, 10-files.MaxPerRun)
	}
}

// Until the apps show delivered files, the tool never tells the run a file is
// in Files or in the chat, in its description or its answer, so a taf never
// sends the owner to a screen the apps don't have. Shown, it says where.
func TestWhereAFileIsSaidOnlyOnceTheAppsShowIt(t *testing.T) {
	for _, shown := range []bool{false, true} {
		d, _ := newTool()
		d.Shown(shown)
		r, err := d.Call(ctx, input("week.csv"))
		if err != nil || r.IsError {
			t.Fatalf("shown %v: %v %q", shown, err, r.Text)
		}
		said := d.Spec().Description + "\n" + r.Text
		for _, where := range []string{"kept 30 days in Files", "it's in Files for 30 days", "it shows in this chat"} {
			if strings.Contains(said, where) != shown {
				t.Errorf("shown %v: %q said: %v\n%s", shown, where, strings.Contains(said, where), said)
			}
		}
		if !shown && !strings.Contains(said, "never that it's in Files, in this chat or anywhere to open") {
			t.Errorf("not shown: the description doesn't tell the run what not to say:\n%s", said)
		}
	}
}
