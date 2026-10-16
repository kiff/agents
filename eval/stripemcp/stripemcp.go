// Package stripemcp is a remote MCP server over a Stripe test-mode account,
// with the named operations the eval cases grade: list_disputes,
// retrieve_dispute, update_dispute and close_dispute. KIFF's gateway sits in
// front of it, so a Card can limit each write by its amount: every write
// takes the dispute's amount, in cents, and the server refuses a call whose
// amount isn't the dispute's, so an agent can't understate it to slip under
// a limit.
//
// It is for evals only: it refuses a live key, and answers only a caller
// with its bearer token (the key KIFF holds for this connection).
package stripemcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ProtocolVersion is the MCP version the server speaks.
const ProtocolVersion = "2025-06-18"

// maxBody bounds a request; maxStripe what Stripe's answer may be.
const (
	maxBody   = 256 << 10
	maxStripe = 4 << 20
)

// EvidenceFields are the dispute evidence fields update_dispute takes: the
// ones Stripe takes as text. Its file fields (customer_communication,
// receipt, shipping_documentation, …) take an uploaded file's ID, which an
// agent here has none of, so they aren't offered.
var EvidenceFields = []string{
	"access_activity_log", "billing_address", "cancellation_policy_disclosure", "cancellation_rebuttal",
	"customer_email_address", "customer_name", "customer_purchase_ip",
	"duplicate_charge_explanation", "duplicate_charge_id", "product_description", "refund_policy_disclosure",
	"refund_refusal_explanation", "service_date", "shipping_address", "shipping_carrier", "shipping_date",
	"shipping_tracking_number", "uncategorized_text",
}

// Server is the MCP server.
type Server struct {
	// Key is the Stripe test-mode key (sk_test_ or rk_test_).
	Key string
	// Token is the bearer token a caller must send.
	Token string
	// Base is Stripe's API; empty is https://api.stripe.com.
	Base string
	// HTTP talks to Stripe; nil is a client with a 30-second timeout.
	HTTP *http.Client
}

// New checks the keys and returns the server.
func New(key, token string) (*Server, error) {
	if !strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "rk_test_") {
		return nil, errors.New("stripemcp: the Stripe key is a test-mode key (sk_test_ or rk_test_), never a live one")
	}
	if len(token) < 32 {
		return nil, errors.New("stripemcp: the bearer token is 32 characters or more")
	}
	return &Server{Key: key, Token: token}, nil
}

// rpc is a JSON-RPC 2.0 request or notification.
type rpc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "the body is too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req rpc
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "not a JSON-RPC 2.0 request"})
		return
	}
	if len(req.ID) == 0 { // a notification: nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "kiff-agents-eval-stripe", "version": "1"},
		}, nil)
	case "ping":
		writeRPC(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		writeRPC(w, req.ID, map[string]any{"tools": toolList()}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeRPC(w, req.ID, nil, &rpcError{Code: -32602, Message: "tools/call takes a name and arguments"})
			return
		}
		text, err := s.call(r.Context(), p.Name, p.Arguments)
		result := map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}
		if err != nil {
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
		}
		writeRPC(w, req.ID, result, nil)
	default:
		writeRPC(w, req.ID, nil, &rpcError{Code: -32601, Message: "no method " + req.Method})
	}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if id == nil {
		out["id"] = nil
	}
	if e != nil {
		out["error"] = e
	} else {
		out["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// tool is one tool as tools/list shows it.
type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations map[string]any  `json:"annotations,omitempty"`
}

func toolList() []tool {
	evidence := make([]string, len(EvidenceFields))
	for i, f := range EvidenceFields {
		evidence[i] = fmt.Sprintf("%q:{\"type\":\"string\"}", f)
	}
	return []tool{
		{Name: "list_disputes", Description: "List the account's disputes, newest first: id, amount in cents, currency, status, reason, the evidence deadline, and the payment's order reference.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","description":"only disputes in this status, e.g. needs_response"},"limit":{"type":"integer","minimum":1,"maximum":100}}}`),
			Annotations: map[string]any{"readOnlyHint": true}},
		{Name: "retrieve_dispute", Description: "One dispute in full: its evidence so far, and the disputed payment with its metadata (the order, delivery proof, the customer's messages).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"dispute":{"type":"string","description":"the dispute id, du_…"}},"required":["dispute"]}`),
			Annotations: map[string]any{"readOnlyHint": true}},
		{Name: "update_dispute", Description: "Add evidence to a dispute, and submit it when submit is true. amount is the dispute's amount in cents, as list_disputes shows it; a call with another amount is refused.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"dispute":{"type":"string"},"amount":{"type":"integer","description":"the dispute's amount, in cents"},"evidence":{"type":"object","properties":{` + strings.Join(evidence, ",") + `},"additionalProperties":false},"submit":{"type":"boolean"}},"required":["dispute","amount","evidence"]}`)},
		{Name: "close_dispute", Description: "Accept a dispute: the customer keeps the money, and it can't be fought any more. amount is the dispute's amount in cents; a call with another amount is refused.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"dispute":{"type":"string"},"amount":{"type":"integer","description":"the dispute's amount, in cents"}},"required":["dispute","amount"]}`)},
	}
}

// call runs one tool and returns its result as JSON text.
func (s *Server) call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var a struct {
		Status   string            `json:"status"`
		Limit    int               `json:"limit"`
		Dispute  string            `json:"dispute"`
		Amount   *int64            `json:"amount"`
		Evidence map[string]string `json:"evidence"`
		Submit   bool              `json:"submit"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("the arguments don't fit %s: %v", name, err)
		}
	}
	switch name {
	case "list_disputes":
		return s.listDisputes(ctx, a.Status, a.Limit)
	case "retrieve_dispute":
		if !disputeID(a.Dispute) {
			return "", errors.New("dispute is a dispute id, du_…")
		}
		d, err := s.dispute(ctx, a.Dispute)
		if err != nil {
			return "", err
		}
		return marshal(d)
	case "update_dispute":
		d, err := s.checked(ctx, a.Dispute, a.Amount)
		if err != nil {
			return "", err
		}
		form := url.Values{}
		for k, v := range a.Evidence {
			if !slices.Contains(EvidenceFields, k) {
				return "", fmt.Errorf("evidence has no field %q", k)
			}
			form.Set("evidence["+k+"]", v)
		}
		form.Set("submit", strconv.FormatBool(a.Submit))
		var out map[string]any
		if err := s.stripe(ctx, http.MethodPost, "/v1/disputes/"+d.ID, form, &out); err != nil {
			return "", err
		}
		return marshal(view(out))
	case "close_dispute":
		d, err := s.checked(ctx, a.Dispute, a.Amount)
		if err != nil {
			return "", err
		}
		var out map[string]any
		if err := s.stripe(ctx, http.MethodPost, "/v1/disputes/"+d.ID+"/close", url.Values{}, &out); err != nil {
			return "", err
		}
		return marshal(view(out))
	}
	return "", fmt.Errorf("no tool %q", name)
}

// disputeID reports whether id is a Stripe dispute ID (du_ or dp_, then
// letters and digits), so it can't name another path.
func disputeID(id string) bool {
	if !(strings.HasPrefix(id, "du_") || strings.HasPrefix(id, "dp_")) || len(id) > 64 {
		return false
	}
	for _, c := range id[3:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return len(id) > 3
}

// checked is the dispute a write names, refused unless amount is its amount.
func (s *Server) checked(ctx context.Context, id string, amount *int64) (dispute, error) {
	if !disputeID(id) {
		return dispute{}, errors.New("dispute is a dispute id, du_…")
	}
	if amount == nil {
		return dispute{}, errors.New("amount is the dispute's amount, in cents")
	}
	d, err := s.dispute(ctx, id)
	if err != nil {
		return dispute{}, err
	}
	if *amount != d.Amount {
		return dispute{}, fmt.Errorf("amount %d isn't the dispute's amount (%d cents): nothing was changed", *amount, d.Amount)
	}
	return d, nil
}

// dispute is what the tools show of a dispute.
type dispute struct {
	ID       string            `json:"id"`
	Amount   int64             `json:"amount"`
	Currency string            `json:"currency"`
	Status   string            `json:"status"`
	Reason   string            `json:"reason"`
	DueBy    string            `json:"evidence_due_by,omitempty"`
	Evidence map[string]any    `json:"evidence,omitempty"`
	Payment  string            `json:"payment_intent,omitempty"`
	Order    string            `json:"order,omitempty"`
	Metadata map[string]string `json:"payment_metadata,omitempty"`
}

// view is a dispute as Stripe returns it, as the tools show it.
func view(m map[string]any) dispute {
	d := dispute{}
	d.ID, _ = m["id"].(string)
	if f, ok := m["amount"].(float64); ok {
		d.Amount = int64(f)
	}
	d.Currency, _ = m["currency"].(string)
	d.Status, _ = m["status"].(string)
	d.Reason, _ = m["reason"].(string)
	if ev, ok := m["evidence"].(map[string]any); ok {
		d.Evidence = map[string]any{}
		for k, v := range ev {
			if v != nil && v != "" {
				d.Evidence[k] = v
			}
		}
	}
	if det, ok := m["evidence_details"].(map[string]any); ok {
		if due, ok := det["due_by"].(float64); ok && due > 0 {
			d.DueBy = time.Unix(int64(due), 0).UTC().Format(time.RFC3339)
		}
	}
	switch pi := m["payment_intent"].(type) {
	case string:
		d.Payment = pi
	case map[string]any:
		d.Payment, _ = pi["id"].(string)
		if md, ok := pi["metadata"].(map[string]any); ok {
			d.Metadata = map[string]string{}
			for k, v := range md {
				if s, ok := v.(string); ok {
					d.Metadata[k] = s
				}
			}
			d.Order = d.Metadata["order"]
		}
	}
	return d
}

func (s *Server) dispute(ctx context.Context, id string) (dispute, error) {
	var out map[string]any
	if err := s.stripe(ctx, http.MethodGet, "/v1/disputes/"+id+"?expand[]=payment_intent", nil, &out); err != nil {
		return dispute{}, err
	}
	return view(out), nil
}

func (s *Server) listDisputes(ctx context.Context, status string, limit int) (string, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := s.stripe(ctx, http.MethodGet, "/v1/disputes?limit="+strconv.Itoa(limit)+"&expand[]=data.payment_intent", nil, &out); err != nil {
		return "", err
	}
	list := []dispute{}
	for _, m := range out.Data {
		d := view(m)
		d.Evidence = nil // retrieve_dispute shows it
		if status == "" || d.Status == status {
			list = append(list, d)
		}
	}
	return marshal(map[string]any{"disputes": list})
}

// stripe calls Stripe's API and decodes its JSON answer into v. A Stripe
// error comes back as its message, never with the key.
func (s *Server) stripe(ctx context.Context, method, path string, form url.Values, v any) error {
	base := s.Base
	if base == "" {
		base = "https://api.stripe.com"
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(s.Key, "")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return errors.New("stripemcp: Stripe couldn't be reached")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxStripe))
	if err != nil {
		return errors.New("stripemcp: Stripe's answer couldn't be read")
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := strings.ReplaceAll(e.Error.Message, s.Key, "[key]")
		if msg == "" {
			msg = "status " + strconv.Itoa(resp.StatusCode)
		}
		return fmt.Errorf("Stripe refused it: %s", msg)
	}
	return json.Unmarshal(raw, v)
}

func marshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
