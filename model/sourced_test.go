package model

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type scripted struct {
	name    string
	err     error
	stream  bool
	calls   int
	factor  float64
	lastReq Request
}

func (s *scripted) Turn(_ context.Context, req Request, onText func(string)) (Reply, error) {
	s.calls++
	s.lastReq = req
	if s.stream && onText != nil {
		onText("partly ")
	}
	if s.err != nil {
		return Reply{}, s.err
	}
	return Reply{Usage: Usage{InputTokens: 1_000_000}, PriceFactor: s.factor}, nil
}

// days keeps each day's total under the day, and each account's part of it
// under "<tenant>|<day>".
type days map[string]int64

func (d days) Reserve(_ context.Context, day, tenant string, micro, limit, share int64) (Full, error) {
	if share > 0 && d[tenant+"|"+day]+micro > share {
		return ShareFull, nil
	}
	if d[day]+micro > limit {
		return DayFull, nil
	}
	d[day] += micro
	d[tenant+"|"+day] += micro
	return NotFull, nil
}

func (d days) Add(_ context.Context, day, tenant string, micro int64) error {
	d[day] += micro
	d[tenant+"|"+day] += micro
	return nil
}

// lockedDays is days for turns at once.
type lockedDays struct {
	mu sync.Mutex
	d  days
}

func (l *lockedDays) Reserve(ctx context.Context, day, tenant string, micro, limit, share int64) (Full, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.Reserve(ctx, day, tenant, micro, limit, share)
}

func (l *lockedDays) Add(ctx context.Context, day, tenant string, micro int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.Add(ctx, day, tenant, micro)
}

var errCredits = errors.New("400 invalid_request_error: Your credit balance is too low")

func fallsBack(err error) bool { return errors.Is(err, errCredits) }

func sourced(primaryErr error, spend days, cap int64) (*Sourced, *scripted, *scripted) {
	p := &scripted{name: "anthropic", err: primaryErr}
	b := &scripted{name: "bedrock", factor: 1.1}
	s := &Sourced{Primary: p, Backup: b, Fallback: true, FallsBack: fallsBack,
		Cap: &DailyCap{MicroUSD: cap, Spend: spend}, Now: func() time.Time { return time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC) }}
	return s, p, b
}

var price = Price{Input: 2_000_000}

// A new run's first turn, refused for credits, runs on the backup and says
// so; its cost, at the backup's price, counts against the day's cap.
func TestANewRunFallsBackOnTheCreditRefusal(t *testing.T) {
	spend := days{}
	s, p, b := sourced(errCredits, spend, 5_000_000)
	r, err := s.Turn(context.Background(), Request{Price: price}, nil)
	if err != nil || p.calls != 1 || b.calls != 1 || r.Source != SourceBedrock {
		t.Fatalf("reply %+v, err %v, calls %d/%d", r, err, p.calls, b.calls)
	}
	if spend["2026-11-07"] != 2_200_000 { // $2 of input at 1.1
		t.Fatalf("spend %v", spend)
	}
	// Without the error, the run stays on the primary.
	s, p, b = sourced(nil, days{}, 5_000_000)
	if r, err := s.Turn(context.Background(), Request{}, nil); err != nil || r.Source != SourceAnthropic || b.calls != 0 {
		t.Fatalf("no error: reply %+v, err %v, backup calls %d", r, err, b.calls)
	}
}

// Never: a run under way, a turn that already streamed, an error that
// doesn't fall back, or with the fallback off.
func TestOnlyANewRunsFirstTurnFallsBack(t *testing.T) {
	other := errors.New("400 messages: roles must alternate")
	for _, c := range []struct {
		name string
		req  Request
		err  error
		set  func(*Sourced, *scripted)
	}{
		{"a run under way on the primary", Request{Source: SourceAnthropic}, errCredits, nil},
		{"another error", Request{}, other, nil},
		{"part of the turn streamed", Request{}, errCredits, func(_ *Sourced, p *scripted) { p.stream = true }},
		{"the fallback off", Request{}, errCredits, func(s *Sourced, _ *scripted) { s.Fallback = false }},
	} {
		s, p, b := sourced(c.err, days{}, 5_000_000)
		if c.set != nil {
			c.set(s, p)
		}
		if _, err := s.Turn(context.Background(), c.req, func(string) {}); !errors.Is(err, c.err) || b.calls != 0 {
			t.Errorf("%s: err %v, backup calls %d", c.name, err, b.calls)
		}
	}
	// A run that started on the backup stays there.
	s, p, b := sourced(nil, days{}, 5_000_000)
	if r, _ := s.Turn(context.Background(), Request{Source: SourceBedrock}, nil); r.Source != SourceBedrock || p.calls != 0 || b.calls != 1 {
		t.Fatalf("pinned to the backup: %+v, calls %d/%d", r, p.calls, b.calls)
	}
	// With the source set to the backup, a new run starts there.
	s, p, b = sourced(nil, days{}, 5_000_000)
	s.Source = SourceBedrock
	if r, _ := s.Turn(context.Background(), Request{}, nil); r.Source != SourceBedrock || p.calls != 0 {
		t.Fatalf("source bedrock: %+v, primary calls %d", r, p.calls)
	}
}

// The backup refuses without a cap, and past the day's cap, in its own words,
// with the primary's error kept; the Eval is exempt.
func TestTheBackupKeepsToItsDailyCap(t *testing.T) {
	s, _, b := sourced(errCredits, days{}, 0)
	s.Cap = nil
	if _, err := s.Turn(context.Background(), Request{}, nil); !errors.Is(err, ErrBackupOff) || !errors.Is(err, errCredits) || b.calls != 0 {
		t.Fatalf("no cap: %v, backup calls %d", err, b.calls)
	}
	spend := days{"2026-11-07": 5_000_000}
	s, _, b = sourced(errCredits, spend, 5_000_000)
	if _, err := s.Turn(context.Background(), Request{}, nil); !errors.Is(err, ErrBackupCapped) || b.calls != 0 {
		t.Fatalf("capped: %v, backup calls %d", err, b.calls)
	}
	s.Exempt = true
	if r, err := s.Turn(context.Background(), Request{}, nil); err != nil || r.Source != SourceBedrock {
		t.Fatalf("exempt: %+v, %v", r, err)
	}
	if spend["2026-11-07"] != 5_000_000 {
		t.Fatalf("an exempt turn counted: %v", spend)
	}
}

// No one account can spend the day for everyone: past its share, its turns
// are refused, as a capped turn is, and another account's still run.
func TestAnAccountKeepsToItsShareOfTheDay(t *testing.T) {
	spend := days{}
	s, _, b := sourced(errCredits, spend, 10_000_000)
	s.Cap.AccountMicroUSD = 3_000_000
	// Each turn reserves 1 micro-USD and settles to $1.10 (scripted).
	req := Request{Tenant: "t-loop", Price: Price{Input: 1_000_000}, MaxTokens: 1}
	ran := 0
	var err error
	for ; ran < 10; ran++ {
		if _, err = s.Turn(context.Background(), req, nil); err != nil {
			break
		}
	}
	if ran != 3 || !errors.Is(err, ErrBackupShareSpent) || !errors.Is(err, ErrBackupCapped) {
		t.Fatalf("%d ran, then %v; want 3, then ErrBackupShareSpent, a capped turn", ran, err)
	}
	calls := b.calls
	other := req
	other.Tenant = "t-other"
	if r, err := s.Turn(context.Background(), other, nil); err != nil || r.Source != SourceBedrock || b.calls != calls+1 {
		t.Fatalf("another account's turn: %+v, %v", r, err)
	}
	if spend["t-loop|2026-11-07"] != 3_300_000 || spend["2026-11-07"] != 4_400_000 {
		t.Fatalf("spend %v, want the day the sum of each account's", spend)
	}
}

// The reviewers' slice is inside the cap: the others stop short of it, a
// reviewer's turn still runs there, with no share of its own, and no turn
// passes the cap itself.
func TestTheReviewersSliceIsKeptForThem(t *testing.T) {
	spend := days{"2026-11-07": 8_000_000}
	s, _, _ := sourced(errCredits, spend, 10_000_000)
	s.Cap.ReservedMicroUSD, s.Cap.Reserved = 2_000_000, map[string]bool{"t-review": true}
	req := Request{Tenant: "t-anyone", Price: Price{Input: 1_000_000}, MaxTokens: 1}
	if _, err := s.Turn(context.Background(), req, nil); !errors.Is(err, ErrBackupCapped) || errors.Is(err, ErrBackupShareSpent) {
		t.Fatalf("another account at the slice: %v, want the day's cap", err)
	}
	req.Tenant = "t-review"
	if r, err := s.Turn(context.Background(), req, nil); err != nil || r.Source != SourceBedrock {
		t.Fatalf("a reviewer in the slice: %+v, %v", r, err)
	}
	req.Tenant = "" // a turn for no account is never a reviewer's
	if _, err := s.Turn(context.Background(), req, nil); !errors.Is(err, ErrBackupCapped) {
		t.Fatalf("no account in the slice: %v", err)
	}
	// A reviewer has no share of its own: past what another account's share
	// would be, its turns still run.
	s.Cap.AccountMicroUSD = 1
	req.Tenant = "t-review"
	if _, err := s.Turn(context.Background(), req, nil); err != nil {
		t.Fatalf("a reviewer past an account's share: %v", err)
	}
	req.Tenant = "t-anyone"
	spend["2026-11-07"], spend["t-anyone|2026-11-07"] = 0, 1
	if _, err := s.Turn(context.Background(), req, nil); !errors.Is(err, ErrBackupShareSpent) {
		t.Fatalf("another account past its share: %v", err)
	}
	spend["2026-11-07"] = 10_000_000
	req.Tenant = "t-review"
	if _, err := s.Turn(context.Background(), req, nil); !errors.Is(err, ErrBackupCapped) {
		t.Fatalf("a reviewer past the cap: %v", err)
	}
}

func TestACostAtTheBackupsPrice(t *testing.T) {
	r := Reply{Usage: Usage{InputTokens: 1_000_000}, PriceFactor: 1.1}
	if got := r.Cost(Price{Input: 2_000_000}); got != 2_200_000 {
		t.Fatalf("cost %d", got)
	}
	if got := (Reply{Usage: Usage{InputTokens: 1_000_000}}).Cost(Price{Input: 2_000_000}); got != 2_000_000 {
		t.Fatalf("no factor: %d", got)
	}
}

// A turn's worst case: every byte read as a token at the dearest input price,
// deferred tools included, the API's tool instructions, plus all its output,
// at the backup's factor.
func TestATurnsWorstCase(t *testing.T) {
	p := Price{Input: 3_000_000, Output: 15_000_000, CacheWrite: 3_750_000, CacheWrite1h: 6_000_000}
	req := Request{System: strings.Repeat("s", 1_000), MaxTokens: 2_000,
		Tools:    []ToolSpec{{Name: "a", Description: strings.Repeat("d", 499), InputSchema: []byte("{}")}, {Name: "b", Defer: true, InputSchema: []byte(strings.Repeat("x", 500))}},
		Messages: []Message{{Role: User, Blocks: []Block{{Kind: Text, Text: strings.Repeat("m", 3_000)}}}}}
	// 1,000 + 502 + 501 + 3,000 bytes, and the tool instructions, at the hour's cache write ($6/M); 2,000 out at $15/M.
	in := int64(1_000+1+499+2+1+500+3_000) + ToolPromptAllowance
	want := (in*6_000_000 + 2_000*15_000_000 + 999_999) / 1_000_000
	if got := WorstCase(req, p, 1, 0); got != want {
		t.Fatalf("worst case %d, want %d", got, want)
	}
	if got := WorstCase(req, p, 1.1, 0); got != int64(math.Ceil(float64(want)*1.1)) {
		t.Fatalf("at 1.1: %d", got)
	}
	// No MaxTokens: a whole turn's output; no prices: still a credit's room.
	if got := WorstCase(Request{}, Price{Output: 1_000_000}, 1, 0); got != MaxOutput {
		t.Fatalf("no MaxTokens: %d, want %d", got, MaxOutput)
	}
	if got := WorstCase(Request{}, Price{}, 1, 0); got != 1 {
		t.Fatalf("unpriced: %d", got)
	}
}

// Files count their own allowance: a PDF many times its bytes, an image at
// least ImageAllowance (a tool's included), text a token a byte; and the
// context window bounds it all, since the model reads no more.
func TestFilesInATurnsWorstCase(t *testing.T) {
	p := Price{Input: 1_000_000} // a token is a micro-dollar: the worst case reads as tokens
	file := func(mediaType string, n int) Request {
		return Request{Messages: []Message{{Role: User, Blocks: []Block{{Kind: File, MediaType: mediaType, Bytes: make([]byte, n)}}}}}
	}
	for _, c := range []struct {
		name string
		req  Request
		want int64
	}{
		{"a PDF", file("application/pdf", 2_000), 2_000 * PDFTokensPerByte},
		{"a small PDF", file("application/pdf", 100), PDFFloor},
		{"a photo", file("image/jpeg", 10_000), 10_000},
		{"a plain image, small", file("image/png", 500), ImageAllowance},
		{"a text file", file(FileText, 1_000), 1_000},
		{"a file no longer carried", file("application/pdf", 0), 1},
		{"a tool's screenshot", Request{Messages: []Message{{Role: User, Blocks: []Block{{Kind: ToolResult, Result: "ok",
			Images: []Image{{MediaType: "image/png", Bytes: make([]byte, 500)}}}}}}}, 2 + ImageAllowance},
	} {
		if got := WorstCase(c.req, p, 1, 0); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	// A 5 MB PDF reserves for its pages, up to what the model can read.
	big := file("application/pdf", 5<<20)
	if got := WorstCase(big, p, 1, 0); got != int64(5<<20)*PDFTokensPerByte {
		t.Fatalf("a 5 MB PDF: %d", got)
	}
	if got := WorstCase(big, p, 1, 1_000_000); got != 1_000_000 {
		t.Fatalf("a 5 MB PDF in a 1M window: %d", got)
	}
}

// The backup's window bounds its reservation, and a turn that cost more than
// its worst case is said.
func TestTheBackupsWindowAndABrokenBound(t *testing.T) {
	spend := &recording{days: days{}}
	b := &windowed{scripted: scripted{name: "bedrock", factor: 1}, window: 10}
	var said []string
	s := &Sourced{Primary: &scripted{err: errCredits}, Backup: b, Fallback: true, FallsBack: fallsBack,
		Cap: &DailyCap{MicroUSD: 50_000_000, Spend: spend}, Now: func() time.Time { return time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC) },
		Log: func(msg string, _ error) { said = append(said, msg) }}
	req := Request{Price: Price{Input: 1_000_000}, MaxTokens: 1, Messages: []Message{{Role: User, Blocks: []Block{{Kind: Text, Text: strings.Repeat("m", 5_000)}}}}}
	if _, err := s.Turn(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if spend.reserved != 10 { // the window's 10 tokens, not the 5,000 bytes; no output price
		t.Fatalf("reserved %d, want the window's 10", spend.reserved)
	}
	// The scripted turn read a million tokens, past its worst case: said, and counted.
	if len(said) != 1 || said[0] != "model: a Bedrock turn cost more than its worst case" || spend.days["2026-11-07"] != 1_000_000 {
		t.Fatalf("said %q, spend %v", said, spend.days)
	}
}

// recording is days that remembers the last reservation asked.
type recording struct {
	days
	reserved int64
}

func (r *recording) Reserve(ctx context.Context, day, tenant string, micro, limit, share int64) (Full, error) {
	r.reserved = micro
	return r.days.Reserve(ctx, day, tenant, micro, limit, share)
}

// windowed is a scripted backup that knows its context window.
type windowed struct {
	scripted
	window int64
}

func (w *windowed) Window(context.Context, string) (int64, error) { return w.window, nil }

// A turn that fails gives its reservation back; one that runs is counted at
// what it cost, not at its worst case.
func TestATurnSettlesToWhatItCost(t *testing.T) {
	spend := days{}
	s, _, b := sourced(errCredits, spend, 50_000_000)
	b.err = errors.New("503 bedrock unavailable")
	req := Request{Price: Price{Input: 2_000_000, Output: 10_000_000}, MaxTokens: 1_000}
	if _, err := s.Turn(context.Background(), req, nil); err == nil || spend["2026-11-07"] != 0 {
		t.Fatalf("a failed turn: %v, spend %v", err, spend)
	}
	b.err = nil
	if _, err := s.Turn(context.Background(), req, nil); err != nil || spend["2026-11-07"] != 2_200_000 {
		t.Fatalf("a turn: %v, spend %v, want its cost, not its worst case", err, spend)
	}
}

// Turns that start together can't pass the cap between them: each reserves
// its worst case first, and those that don't fit are refused before running.
func TestTurnsAtOnceKeepToTheCap(t *testing.T) {
	req := Request{Price: Price{Input: 2_000_000, Output: 10_000_000}, MaxTokens: 1_000}
	worst := WorstCase(req, req.Price, 1.1, 0)
	spend := &lockedDays{d: days{}}
	start := make(chan struct{})
	b := &blocking{start: start}
	s := &Sourced{Primary: refusing{}, Backup: b, Fallback: true, FallsBack: fallsBack,
		Cap: &DailyCap{MicroUSD: 3*worst + 1, Spend: spend, PriceFactor: 1.1}, Now: func() time.Time { return time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC) }}
	const n = 20
	errs := make(chan error, n)
	for range n {
		go func() { _, err := s.Turn(context.Background(), req, nil); errs <- err }()
	}
	refused := 0
	for range n - 3 { // the refused don't wait for the turns that run
		if err := <-errs; !errors.Is(err, ErrBackupCapped) {
			t.Fatalf("a turn past the cap: %v", err)
		}
		refused++
	}
	close(start)
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if b.calls.Load() != 3 || refused != n-3 || spend.d["2026-11-07"] != 3*2_200_000 {
		t.Fatalf("%d ran, %d refused, spend %v", b.calls.Load(), refused, spend.d)
	}
}

// refusing is a primary that refuses every turn for credits, safe to call at once.
type refusing struct{}

func (refusing) Turn(context.Context, Request, func(string)) (Reply, error) {
	return Reply{}, errCredits
}

// blocking is a backup whose turns wait for start, each costing $2 of input.
type blocking struct {
	start chan struct{}
	calls atomic.Int32
}

func (b *blocking) Turn(context.Context, Request, func(string)) (Reply, error) {
	b.calls.Add(1)
	<-b.start
	return Reply{Usage: Usage{InputTokens: 1_000_000}, PriceFactor: 1.1}, nil
}

// An account's share follows its plan as KIFF said it: the trial's small
// one, a paying plan's balance with room to spare and never below the flat
// share, an unbounded account's ceiling, and the flat share when KIFF said
// nothing it knows. None passes what every account but the reviewers' may
// take of the day, and a reviewer has none.
func TestAnAccountsShareFollowsItsPlan(t *testing.T) {
	c := &DailyCap{MicroUSD: 500_000_000, AccountMicroUSD: 50_000_000, TrialMicroUSD: 3_000_000, MaxMicroUSD: 200_000_000,
		ReservedMicroUSD: 100_000_000, Reserved: map[string]bool{"t-review": true}}
	for _, x := range []struct {
		tenant string
		plan   Plan
		want   int64
	}{
		{"t1", Plan{}, 50_000_000},
		{"t1", Plan{Kind: PlanNone}, 3_000_000},
		{"t1", Plan{Kind: PlanSolo, PeriodMicroUSD: 10_000_000}, 50_000_000},        // the floor
		{"t1", Plan{Kind: PlanSolo, PeriodMicroUSD: 80_000_000}, 108_000_000},       // 80 + 8 + 20
		{"t1", Plan{Kind: PlanCompany, PeriodMicroUSD: 160_000_000}, 196_000_000},   // a Team at its limit
		{"t1", Plan{Kind: PlanCompany, PeriodMicroUSD: 1_000_000_000}, 400_000_000}, // the day less the slice
		{"t1", Plan{Kind: PlanSolo}, 50_000_000},                                    // no balance said: flat
		{"t1", Plan{Kind: PlanUnbounded}, 200_000_000},
		{"t1", Plan{Kind: "gold", PeriodMicroUSD: 900_000_000}, 50_000_000}, // a kind not known: flat
		{"t-review", Plan{Kind: PlanSolo, PeriodMicroUSD: 80_000_000}, 0},
	} {
		if got := c.Share(x.tenant, x.plan); got != x.want {
			t.Errorf("%s %+v: share %d, want %d", x.tenant, x.plan, got, x.want)
		}
	}
	c.MaxMicroUSD, c.TrialMicroUSD = 0, 0
	if got := c.Share("t1", Plan{Kind: PlanUnbounded}); got != 50_000_000 {
		t.Errorf("unbounded with no ceiling set: %d, want the flat share", got)
	}
	if got := c.Share("t1", Plan{Kind: PlanNone}); got != 50_000_000 {
		t.Errorf("the trial with no share set: %d, want the flat share", got)
	}
	c.AccountMicroUSD = 0
	if got := c.Share("t1", Plan{Kind: PlanSolo, PeriodMicroUSD: 80_000_000}); got != 0 {
		t.Errorf("no shares at all: %d", got)
	}
}

// A paying account with credits left isn't stopped by its share: a Solo
// whose period can pay for $80 runs past the flat $50, and stops only past
// its own share. The same day without a plan said stops at $50.
func TestAPayingAccountWithCreditsLeftIsntStoppedByItsShare(t *testing.T) {
	spend := days{"2026-11-07": 60_000_000, "t-solo|2026-11-07": 60_000_000}
	s, _, _ := sourced(errCredits, spend, 500_000_000)
	s.Cap.AccountMicroUSD = 50_000_000
	req := Request{Tenant: "t-solo", Plan: Plan{Kind: PlanSolo, PeriodMicroUSD: 80_000_000}, Price: Price{Input: 1_000_000}, MaxTokens: 1}
	if r, err := s.Turn(context.Background(), req, nil); err != nil || r.Source != SourceBedrock {
		t.Fatalf("past $50 with credits left: %+v, %v", r, err)
	}
	spend["t-solo|2026-11-07"] = 108_000_000
	if _, err := s.Turn(context.Background(), req, nil); !errors.Is(err, ErrBackupShareSpent) {
		t.Fatalf("past its own share: %v", err)
	}
	spend["t-solo|2026-11-07"] = 60_000_000
	req.Plan = Plan{}
	if _, err := s.Turn(context.Background(), req, nil); !errors.Is(err, ErrBackupShareSpent) {
		t.Fatalf("no plan said, past $50: %v", err)
	}
}

// Twenty Coach-sized turns at once, when the account's balance is all but
// used, all run: the share's room beyond the balance holds their worst cases,
// for a plan between the floor and where a tenth would cover them alone.
func TestTwentyTurnsAtOnceAtTheEndOfABalanceAllRun(t *testing.T) {
	req := Request{Tenant: "t-team", Plan: Plan{Kind: PlanCompany, PeriodMicroUSD: 45_000_000},
		Price: Price{Input: 3_000_000, Output: 15_000_000}, MaxTokens: 48_000}
	worst := WorstCase(req, req.Price, 1.1, 0)
	if worst < 790_000 {
		t.Fatalf("a worst case of %d micro-USD is no Coach turn's", worst)
	}
	// The balance is used up to its last cent: what's spent today is what
	// the period could pay for.
	spend := &lockedDays{d: days{"2026-11-07": 45_000_000, "t-team|2026-11-07": 45_000_000}}
	start := make(chan struct{})
	b := &blocking{start: start}
	s := &Sourced{Primary: refusing{}, Backup: b, Fallback: true, FallsBack: fallsBack,
		Cap: &DailyCap{MicroUSD: 500_000_000, AccountMicroUSD: 50_000_000, Spend: spend, PriceFactor: 1.1},
		Now: func() time.Time { return time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC) }}
	const n = 20
	errs := make(chan error, n)
	for range n {
		go func() { _, err := s.Turn(context.Background(), req, nil); errs <- err }()
	}
	for b.calls.Load() < n {
		select {
		case err := <-errs:
			t.Fatalf("a turn refused with the balance's room left: %v", err)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(start)
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
