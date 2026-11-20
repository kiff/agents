package server_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kiff/agents/schedule"
)

// tools is an agent's reads as a check would find them: a read it has, one that isn't a read, and a
// list that can't be read now; anything else it has none of.
func tools(ctx context.Context, tenant, agent, name string) error {
	switch name {
	case "calendar__list_events":
		return nil
	case "mail__send":
		return &schedule.CheckError{Reason: schedule.ReasonNotARead, Err: errors.New("not a read")}
	case "flaky__list":
		return errors.New("the gateway didn't answer")
	}
	return &schedule.CheckError{Reason: schedule.ReasonNoTool, Err: errors.New("no tool " + name)}
}

// A check's read is asked of the same list a firing reads with, before it's set: a read the agent
// has is fine, one it surely hasn't is a 409 no_tool (or not_a_read) naming it, and a list that
// can't be read now never refuses. A check that kept reading list_emails after its Gmail connection went
// failed every firing with no_tool (Nov 2026).
func TestCheckToolSaysWhetherTheAgentHasTheRead(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "inbox")
	ask := func(tool string) (int, map[string]any) {
		return f.do("t1", "POST", "/agents/inbox/check-tool", map[string]any{"tool": tool})
	}
	// No list to ask: nothing is checked, never a refusal.
	code, body := ask("mail__list_emails")
	f.want(200, code, body, "no HasTool")
	if body["checked"] != false || body["has"] != nil {
		t.Fatalf("without HasTool: %v", body)
	}
	f.server.Schedules.HasTool = tools
	code, body = ask("calendar__list_events")
	f.want(200, code, body, "a read it has")
	if body["has"] != true || body["tool"] != "calendar__list_events" {
		t.Fatalf("has: %v", body)
	}
	code, body = ask("mail__list_emails")
	f.want(409, code, body, "a read it hasn't")
	if body["error"] != "no_tool" || !strings.Contains(body["message"].(string), "mail__list_emails") {
		t.Fatalf("no_tool: %v", body)
	}
	code, body = ask("mail__send")
	f.want(409, code, body, "not a read")
	if body["error"] != "not_a_read" {
		t.Fatalf("not_a_read: %v", body)
	}
	code, body = ask("flaky__list")
	f.want(200, code, body, "a list that can't be read")
	if body["checked"] != false || body["has"] != nil {
		t.Fatalf("unread list: %v", body)
	}
	code, body = ask("")
	f.want(400, code, body, "no tool named")
	code, body = f.do("t1", "POST", "/agents/gone/check-tool", map[string]any{"tool": "calendar__list_events"})
	f.want(404, code, body, "an agent the tenant hasn't")
}

// A schedule's update that sets its check to another read refuses one the agent surely hasn't, and
// keeps the schedule as it was; one that keeps reading what it read is never refused, so a check
// whose read is gone can still be paused or moved.
func TestAScheduleUpdateRefusesACheckOfAReadTheAgentHasnt(t *testing.T) {
	f := newRunFixture(t)
	f.putAgent("t1", "inbox")
	sc := scheduleBody("morning")
	sc.Agent = "inbox"
	sc.AppCheck = &schedule.Check{Tool: "mail__list_emails", Items: "messages", Cursor: "internalDate"}
	code, body := f.do("t1", "POST", "/schedules", sc)
	f.want(201, code, body, "create")
	f.server.Schedules.HasTool = tools

	// The same read, other times: not checked again.
	moved := sc
	moved.Cron = "0 9 * * *"
	code, body = f.do("t1", "PUT", "/schedules/morning", moved)
	f.want(200, code, body, "the same read, moved")

	other := moved
	other.AppCheck = &schedule.Check{Tool: "mail__list_threads", Items: "threads", Cursor: "id"}
	code, body = f.do("t1", "PUT", "/schedules/morning", other)
	f.want(409, code, body, "another read the agent hasn't")
	if body["error"] != "no_tool" || !strings.Contains(body["message"].(string), "mail__list_threads") {
		t.Fatalf("refused: %v", body)
	}
	if got, _ := f.server.Schedules.Store.Get(ctx, "t1", "morning"); got.AppCheck == nil || got.AppCheck.Tool != "mail__list_emails" || got.Cron != "0 9 * * *" {
		t.Fatalf("a refused update changed the schedule: %+v", got)
	}

	// The fix: a read it has.
	fixed := moved
	fixed.AppCheck = &schedule.Check{Tool: "calendar__list_events", Items: "items", Cursor: "updated"}
	code, body = f.do("t1", "PUT", "/schedules/morning", fixed)
	f.want(200, code, body, "a read it has")
	if got, _ := f.server.Schedules.Store.Get(ctx, "t1", "morning"); got.AppCheck == nil || got.AppCheck.Tool != "calendar__list_events" {
		t.Fatalf("not set: %+v", got.AppCheck)
	}

	// The same read, moved to another agent that hasn't it: refused, and the schedule stays.
	f.putAgent("t1", "calendar")
	f.server.Schedules.HasTool = func(ctx context.Context, tenant, agent, name string) error {
		if agent == "calendar" {
			return &schedule.CheckError{Reason: schedule.ReasonNoTool, Err: errors.New("no tool " + name)}
		}
		return tools(ctx, tenant, agent, name)
	}
	elsewhere := fixed
	elsewhere.Agent = "calendar"
	code, body = f.do("t1", "PUT", "/schedules/morning", elsewhere)
	f.want(409, code, body, "the same read on an agent that hasn't it")
	if body["error"] != "no_tool" {
		t.Fatalf("moved: %v", body)
	}
	if got, _ := f.server.Schedules.Store.Get(ctx, "t1", "morning"); got.Agent != "inbox" || got.AppCheck == nil || got.AppCheck.Tool != "calendar__list_events" {
		t.Fatalf("a refused move changed the schedule: %+v", got)
	}
	f.server.Schedules.HasTool = tools

	// A list that can't be read now never refuses.
	flaky := fixed
	flaky.AppCheck = &schedule.Check{Tool: "flaky__list", Items: "data", Cursor: "id"}
	code, body = f.do("t1", "PUT", "/schedules/morning", flaky)
	f.want(200, code, body, "a list that can't be read")
}
