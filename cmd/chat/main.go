// Command chat talks to an agent from the terminal, through KIFF's gateway,
// with sessions kept in memory. It's for trying an agent against KIFF in test
// mode, not for production.
//
//	KIFF_AGENT_KEY=… ANTHROPIC_API_KEY=… go run ./cmd/chat -model <model id> -price 5000000,25000000
//
// Each line you type is one message. Keys are read from the environment only.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kiff/agents/agent"
	"github.com/kiff/agents/chat"
	"github.com/kiff/agents/model"
	"github.com/kiff/agents/model/anthropic"
	"github.com/kiff/agents/session"
	"github.com/kiff/agents/store/memory"
	"github.com/kiff/agents/tool/kiff"
	toolmemory "github.com/kiff/agents/tool/memory"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "chat:", err)
		os.Exit(1)
	}
}

func run() error {
	modelID := flag.String("model", "", "the model id (required)")
	price := flag.String("price", "", "input,output price in millionths of a dollar per million tokens (required)")
	maxCost := flag.Float64("max-cost", 1, "the most one message's run may cost, in US dollars")
	gateway := flag.String("gateway", kiff.URL, "KIFF's gateway")
	system := flag.String("system", "", "a file with the agent's instructions")
	skill := flag.String("skill", "", "a file with KIFF's skill, added to the instructions")
	flag.Parse()

	key := os.Getenv("KIFF_AGENT_KEY")
	if key == "" {
		return errors.New("set KIFF_AGENT_KEY to the agent's KIFF key")
	}
	if *modelID == "" {
		return errors.New("-model is required")
	}
	p, err := parsePrice(*price)
	if err != nil {
		return err
	}
	instructions, err := readFiles(*system, *skill)
	if err != nil {
		return err
	}

	ctx := context.Background()
	tools, err := kiff.New(*gateway, key, nil).Tools(ctx)
	if err != nil {
		return err
	}
	store := memory.New()
	tools = append(tools, toolmemory.New(store, "local", "cli"))
	a := agent.Agent{Tenant: "local", ID: "cli", Model: anthropic.New(), ModelID: *modelID, Price: p,
		MaxTokens: 8192, System: instructions, Tools: tools, MaxCost: int64(*maxCost * 1_000_000)}
	if err := a.Check(); err != nil {
		return err
	}
	rec, err := session.NewRecorder(store, session.DefaultPolicy(30*24*time.Hour))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d tools through KIFF. Type a message; Ctrl-D ends.\n", len(tools)-1)
	in := bufio.NewScanner(os.Stdin)
	for fmt.Print("> "); in.Scan(); fmt.Print("> ") {
		text := strings.TrimSpace(in.Text())
		if text == "" {
			continue
		}
		_, out, err := chat.Send(ctx, rec, store, a, text, time.Now(), show)
		fmt.Println()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}
		fmt.Fprintf(os.Stderr, "[%s · $%.4f]\n", out.Stop, float64(out.Cost)/1_000_000)
	}
	return in.Err()
}

func show(e agent.Event) {
	switch e.Kind {
	case agent.TextDelta:
		fmt.Print(e.Text)
	case agent.CallStart:
		fmt.Fprintf(os.Stderr, "\n→ %s %s\n", e.Tool, e.Input)
	case agent.CallResult:
		state := e.Result.State
		if state == "" && e.Result.IsError {
			state = "error"
		}
		if state != "" {
			fmt.Fprintf(os.Stderr, "← %s: %s\n", e.Tool, state)
		}
	}
}

func parsePrice(s string) (model.Price, error) {
	in, out, ok := strings.Cut(s, ",")
	i, err1 := strconv.ParseInt(strings.TrimSpace(in), 10, 64)
	o, err2 := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if !ok || err1 != nil || err2 != nil || i <= 0 || o <= 0 {
		return model.Price{}, errors.New("-price is input,output in millionths of a dollar per million tokens, both positive")
	}
	return model.Price{Input: i, Output: o}, nil
}

func readFiles(paths ...string) (string, error) {
	var parts []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		parts = append(parts, strings.TrimSpace(string(b)))
	}
	return strings.Join(parts, "\n\n"), nil
}
