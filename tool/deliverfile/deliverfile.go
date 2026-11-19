// Package deliverfile is the tool through which a taf's run hands its account
// a file it made: a report, a sheet or a document (package files). The file
// is kept 30 days, for everyone on the account to open, save or share from
// Files. It writes only inside the account, so it's the agent's own tool: no
// Card governs it, and it's never counted with the calls that act outside.
package deliverfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/kiff/agents/files"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/tool"
)

// Name is the tool's name, as the model sees it.
const Name = "deliver_file"

// Sessions says which chat a run is in, when it's in one.
type Sessions func(ctx context.Context, tenant, run string) (string, error)

// Tool is one run's deliveries.
type Tool struct {
	files              *files.Service
	tenant, agent, run string
	session            Sessions
	// shown: the apps show delivered files (their Files switch is on), so the
	// tool may tell the run where people open them. Off, it says only that the
	// file is kept: a taf never sends the owner to a screen the apps don't have.
	shown bool

	mu sync.Mutex
	n  int // files this run delivered through it
}

// New returns the tool for one run of tenant's agent. session, when set,
// names the run's chat, so the file shows there too.
func New(fs *files.Service, tenant, agent, run string, session Sessions) *Tool {
	return &Tool{files: fs, tenant: tenant, agent: agent, run: run, session: session}
}

// Shown returns t telling the run that people open delivered files in the
// apps' Files and the chat, once the apps show them.
func (t *Tool) Shown(on bool) *Tool {
	t.shown = on
	return t
}

// where is where people find a delivered file, as the run may say it.
func (t *Tool) where() string {
	if t.shown {
		return "It's kept 30 days in Files, where everyone on the account can open, save or share it, and it shows in this chat. "
	}
	return "It's kept 30 days for the account. The apps don't show delivered files yet: say what's in the file and that it's kept, " +
		"never that it's in Files, in this chat or anywhere to open. "
}

func (t *Tool) Access() tool.Access { return tool.Own }
func (t *Tool) Gateway() bool       { return false }

func (t *Tool) Spec() model.ToolSpec {
	return model.ToolSpec{
		Name: Name,
		Description: fmt.Sprintf("Hand the account a file you made in this run: a report, a table or a document, as text. "+
			t.where()+
			"media_type is one of text/csv, text/markdown, text/plain or application/json; content is the whole file, at most 10 MB; "+
			"name is what people see (its extension is set from the type). At most %d files a run. "+
			"A CSV cell starting with =, +, - or @ that isn't a number is kept as text, never as a formula. "+
			"Delivering a file sends nothing outside the account: say what's in it, never that it was sent anywhere.", files.MaxPerRun),
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string","description":"the file's name, as people see it"},` +
			`"media_type":{"type":"string","enum":["text/csv","text/markdown","text/plain","application/json"]},` +
			`"content":{"type":"string","description":"the whole file, as UTF-8 text"}},` +
			`"required":["name","media_type","content"]}`),
	}
}

// Call keeps the file. What can't be kept is a Result with IsError saying
// why, so the model can correct it.
func (t *Tool) Call(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	var d files.Delivery
	if err := json.Unmarshal(input, &d); err != nil {
		return tool.Result{Text: "deliver_file takes {name, media_type, content}: " + err.Error(), IsError: true}, nil
	}
	// One run's calls may come at once: the count is this run's, under a lock,
	// so a sixth never gets past the store's check by racing it.
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n >= files.MaxPerRun {
		return tool.Result{Text: fmt.Sprintf("a run delivers at most %d files, and this one has", files.MaxPerRun), IsError: true}, nil
	}
	session := ""
	if t.session != nil {
		s, err := t.session(ctx, t.tenant, t.run)
		if err != nil {
			return tool.Result{}, err
		}
		session = s
	}
	f, err := t.files.Deliver(ctx, t.tenant, t.agent, t.run, session, d)
	if errors.Is(err, files.ErrInvalid) {
		return tool.Result{Text: err.Error(), IsError: true}, nil
	}
	if err != nil {
		return tool.Result{}, err
	}
	t.n++
	if !t.shown {
		return tool.Result{Text: fmt.Sprintf("Delivered %q (%d bytes, id %s): kept 30 days for the account. The apps don't show it yet.", f.Name, f.Size, f.ID)}, nil
	}
	return tool.Result{Text: fmt.Sprintf("Delivered %q (%d bytes, id %s): it's in Files for 30 days, for everyone on the account.", f.Name, f.Size, f.ID)}, nil
}
