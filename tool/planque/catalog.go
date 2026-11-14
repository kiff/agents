package planque

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/kiff/agents/tool"
)

// catalog answers with what the vendor lists for a listed grant: each
// tool's own words and arguments, and the account. No value from the
// account is in it. With only, it is that one tool, in full.
// A call follows at most this many vendor pages, each bounded to 16 MiB,
// and retains at most 16 MiB of catalog data before bounding the reply.
// The model sees at most MaxCatalog; a cursor resumes at the first unlisted
// tool, even when the model's bound falls inside a vendor page.
const (
	MaxCatalogPages  = 20
	maxCatalogWire   = 16 << 20
	maxCatalogCursor = 4096
)

type catalogCursor struct {
	Page   string `json:"page,omitempty"`
	Offset int    `json:"offset,omitempty"`
}

func (c catalogCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

type catalogPage struct {
	Grant     string        `json:"grant"`
	Server    string        `json:"server"`
	Account   string        `json:"account,omitempty"`
	Tools     []catalogTool `json:"tools"`
	Next      string        `json:"next_cursor,omitempty"`
	Skipped   int           `json:"skipped,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
}

// maxMatches is how many tools a word search of a catalog answers at most,
// each in full.
const maxMatches = 20

// matches says whether every word of q is in the tool's name or description,
// whatever the case.
func matches(q string, ct catalogTool) bool {
	text := strings.ToLower(ct.Name + " " + ct.Description)
	for _, w := range strings.Fields(strings.ToLower(q)) {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}

func (t *Tool) catalog(ctx context.Context, path, only, q, cursor string) (tool.Result, error) {
	q = strings.TrimSpace(q)
	if only != "" && q != "" {
		return tool.Result{Text: "catalog takes tool (one by its name) or q (words to search by), not both", IsError: true}, nil
	}
	var at catalogCursor
	if cursor != "" {
		if len(cursor) > maxCatalogCursor {
			return tool.Result{Text: "catalog needs the next_cursor from its previous answer", IsError: true}, nil
		}
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(b, &at) != nil || at.Offset < 0 {
			return tool.Result{Text: "catalog needs the next_cursor from its previous answer", IsError: true}, nil
		}
	}
	var c struct {
		Grant     string        `json:"grant"`
		Server    string        `json:"server"`
		Account   string        `json:"account,omitempty"`
		Tools     []catalogTool `json:"tools"`
		Truncated bool          `json:"truncated,omitempty"`
		Note      string        `json:"note,omitempty"`
		Next      string        `json:"next_cursor,omitempty"`
		Skipped   int           `json:"skipped,omitempty"`
	}
	// Keep each tool's position before bounding: names that don't fit can be
	// recovered by resuming this page at its original offset.
	var positions []catalogCursor
	seen := map[string]bool{}
	total := 0
	for page := 0; page < MaxCatalogPages; page++ {
		if seen[at.Page] {
			return tool.Result{}, fmt.Errorf("planque: catalog cursor did not advance")
		}
		seen[at.Page] = true
		endpoint := path
		if at.Page != "" {
			endpoint += "?cursor=" + url.QueryEscape(at.Page)
		}
		raw, refused, err := t.doBounded(ctx, http.MethodGet, endpoint, nil, maxCatalogWire)
		if err != nil || refused != nil {
			return deref(refused), err
		}
		var p catalogPage
		if err := json.Unmarshal(raw, &p); err != nil || p.Grant == "" || p.Skipped < 0 {
			return tool.Result{}, fmt.Errorf("planque: an answer that isn't a catalog")
		}
		if page == 0 {
			c.Grant, c.Server, c.Account = p.Grant, p.Server, p.Account
		} else if p.Grant != c.Grant || p.Server != c.Server || p.Account != c.Account {
			return tool.Result{}, fmt.Errorf("planque: catalog changed between pages")
		}
		if at.Offset > len(p.Tools) {
			return tool.Result{Text: "the catalog changed; request catalog without a cursor again", IsError: true}, nil
		}
		if page > 0 && total+len(raw) > maxCatalogWire {
			c.Next = at.encode()
			break
		}
		total += len(raw)
		c.Skipped += p.Skipped
		c.Truncated = c.Truncated || p.Truncated
		if only != "" {
			// By its name, or by its provider's slug (the name a guide and a search give a managed app's tool).
			i := slices.IndexFunc(p.Tools[at.Offset:], func(ct catalogTool) bool {
				return ct.Name == only || (ct.Slug != "" && strings.EqualFold(ct.Slug, only))
			})
			if i >= 0 {
				c.Tools = p.Tools[at.Offset+i : at.Offset+i+1]
				// This is one tool in full, rather than a partial catalog.
				c.Truncated, c.Skipped = false, 0
				break
			}
		} else if q != "" {
			// A word search across the pages (kiff/tafs#476, gap 1): the tools
			// whose name or description has every word, in full, up to
			// maxMatches; a cursor resumes just after the last match.
			full := false
			for i := at.Offset; i < len(p.Tools); i++ {
				if !matches(q, p.Tools[i]) {
					continue
				}
				c.Tools = append(c.Tools, p.Tools[i])
				if len(c.Tools) == maxMatches {
					if i+1 < len(p.Tools) {
						c.Next = catalogCursor{Page: at.Page, Offset: i + 1}.encode()
					} else if p.Next != "" {
						c.Next = catalogCursor{Page: p.Next}.encode()
					}
					full = true
					break
				}
			}
			if full {
				break
			}
		} else {
			for i := at.Offset; i < len(p.Tools); i++ {
				positions = append(positions, catalogCursor{Page: at.Page, Offset: i})
			}
			c.Tools = append(c.Tools, p.Tools[at.Offset:]...)
		}
		if p.Next == "" {
			break
		}
		at = catalogCursor{Page: p.Next}
		if len(at.encode()) > maxCatalogCursor-64 { // leave room for an offset
			return tool.Result{}, fmt.Errorf("planque: catalog cursor is too large")
		}
		if page == MaxCatalogPages-1 {
			c.Next = at.encode()
		}
	}
	incompleteSearch := false
	if q != "" {
		var shortened, left int
		header := c
		header.Tools = nil
		metadata, _ := json.Marshal(header)
		c.Tools, shortened, left = bound(c.Tools, MaxCatalog-len(metadata)-maxCatalogCursor-512)
		c.Truncated = c.Truncated || c.Next != "" || left > 0
		switch {
		case len(c.Tools) == 0 && !c.Truncated && c.Skipped == 0:
			c.Note = "No tool's name or description has every word of " + strconv.Quote(q) + ": try fewer or other words, or catalog {grant} to list them."
		case len(c.Tools) == 0:
			c.Note = "No match so far; the search is incomplete, so a tool is not known to be absent."
			if c.Next != "" {
				c.Note += " Continue catalog {grant, q, cursor: next_cursor}."
			}
			incompleteSearch = true
		default:
			c.Note = fmt.Sprintf("%d tools match %s, in full.", len(c.Tools), strconv.Quote(q))
			if len(c.Tools) == 1 {
				c.Note = fmt.Sprintf("1 tool matches %s, in full.", strconv.Quote(q))
			}
			if shortened > 0 {
				c.Note += fmt.Sprintf(" %d are shortened: catalog {grant, tool} shows one in full.", shortened)
			}
			if c.Next != "" {
				c.Note += " More may match: continue catalog {grant, q, cursor: next_cursor}."
			}
		}
	} else if only != "" {
		if len(c.Tools) == 0 {
			if c.Next == "" && !c.Truncated && c.Skipped == 0 {
				return tool.Result{Text: "the catalog has no tool named " + only + "; catalog {grant, q} finds its tools by words", IsError: true}, nil
			}
			c.Truncated = true
			c.Note = "The search is incomplete; the tool is not known to be absent."
			if c.Next != "" {
				c.Note += " Continue catalog {grant, tool, cursor: next_cursor}."
			}
			incompleteSearch = true
		}
	} else {
		var shortened, left int
		header := c
		header.Tools = nil
		metadata, _ := json.Marshal(header)
		c.Tools, shortened, left = bound(c.Tools, MaxCatalog-len(metadata)-maxCatalogCursor-512)
		if left > 0 {
			c.Next = positions[len(c.Tools)].encode()
		}
		c.Truncated = c.Truncated || c.Next != "" || c.Skipped > 0
		c.Note = catalogNote(len(c.Tools), shortened, left, c.Truncated)
		if c.Next != "" {
			c.Note = fmt.Sprintf("%d tools are listed. More remain: continue catalog {grant, cursor: next_cursor}. %d listed tools are shortened: catalog {grant, tool} shows one in full.", len(c.Tools), shortened)
		}
		if c.Skipped > 0 {
			c.Note += fmt.Sprintf(" The vendor skipped %d entries.", c.Skipped)
		}
		if cursor != "" && !c.Truncated {
			c.Note = fmt.Sprintf("All remaining tools are listed (%d). %d are shortened: catalog {grant, tool} shows one in full.", len(c.Tools), shortened)
		}
	}
	b, _ := json.Marshal(c)
	if len(b) > MaxCatalog {
		return tool.Result{Text: "this tool's description and arguments are larger than the catalog may be", IsError: true}, nil
	}
	return tool.Result{Text: string(b), IsError: incompleteSearch}, nil
}
