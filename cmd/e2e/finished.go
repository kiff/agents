package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/kiff/agents/cmd/internal/testaccount"
)

// finishedCalls is the most calls one -finished-grants run makes: planque
// deletes at most 200 grants a call, so this is 4,000 grants.
const finishedCalls = 20

// runFinishedGrants is -finished-grants: the test account's finished grants
// (disconnected or failed) deleted with their own records, through planque's
// own route, signed by the owner's phone as the account's erase is. planque
// decides what is finished and skips one that still holds a secret; this
// only calls until it says there's no more, and reads the account before and
// after. A live grant that is gone after, or one more call than the cap,
// fails the run. The log is public: counts only, never a grant's id.
func runFinishedGrants(ctx context.Context, apiBase, planqueBase string, cfg config, out io.Writer) error {
	phone, err := testPhone(ctx, apiBase, planqueBase, cfg, "-finished-grants")
	if err != nil {
		return err
	}
	return deleteFinishedGrants(ctx, phone, out)
}

func deleteFinishedGrants(ctx context.Context, phone *testaccount.Phone, out io.Writer) error {
	before, err := everyGrant(ctx, phone)
	if err != nil {
		return fmt.Errorf("the test account's grants can't be read: %w", err)
	}
	fmt.Fprintf(out, "before: %s\n", grantCounts(before))
	var grants, ops, skipped int
	for call := 1; ; call++ {
		if call > finishedCalls {
			return fmt.Errorf("planque still has more after %d calls: run it again", finishedCalls)
		}
		a, err := phone.Planque.Do(ctx, http.MethodDelete, phone.AccountPath()+"/finished-grants", nil)
		if err != nil {
			return fmt.Errorf("call %d: %w", call, err)
		}
		if err := a.Err(); err != nil {
			return fmt.Errorf("call %d: %w", call, err)
		}
		var got struct {
			Grants     int  `json:"grants"`
			Operations int  `json:"operations"`
			Skipped    int  `json:"skipped"`
			More       bool `json:"more"`
		}
		if err := json.Unmarshal([]byte(a.Raw), &got); err != nil {
			return fmt.Errorf("call %d: planque's answer: %w", call, err)
		}
		grants, ops, skipped = grants+got.Grants, ops+got.Operations, got.Skipped
		fmt.Fprintf(out, "call %d: %d grants deleted, %d operations, %d skipped\n", call, got.Grants, got.Operations, got.Skipped)
		if !got.More {
			break
		}
	}
	after, err := everyGrant(ctx, phone)
	if err != nil {
		return fmt.Errorf("the test account's grants after: %w", err)
	}
	fmt.Fprintf(out, "after: %s\n", grantCounts(after))
	fmt.Fprintf(out, "deleted: %d grants, %d operations; %d finished grants kept, each still holding a secret\n", grants, ops, skipped)
	if l, k := liveGrants(before), liveGrants(after); k != l {
		return fmt.Errorf("the live grants changed: %d before, %d after", l, k)
	}
	return nil
}

// liveGrants is how many grants are neither disconnected nor failed.
func liveGrants(gs []testaccount.Grant) int {
	n := 0
	for _, g := range gs {
		if g.Status != "disconnected" && g.Status != "failed" {
			n++
		}
	}
	return n
}

// grantCounts says how many grants there are, by status.
func grantCounts(gs []testaccount.Grant) string {
	by := map[string]int{}
	for _, g := range gs {
		by[g.Status]++
	}
	names := make([]string, 0, len(by))
	for s := range by {
		names = append(names, s)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, s := range names {
		parts = append(parts, fmt.Sprintf("%d %s", by[s], s))
	}
	if len(parts) == 0 {
		return "0 grants"
	}
	return fmt.Sprintf("%d grants (%s)", len(gs), strings.Join(parts, ", "))
}
