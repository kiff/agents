package anthropic

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
)

// FallsBack says whether err, from a turn on the Claude API, lets a new run
// go to the backup (model.Sourced):
//   - the credit refusal, matched on all of its status (400), its error type
//     (invalid_request_error) and its words ("credit balance");
//   - the API unavailable once the SDK's own retries are spent (500, 502,
//     503, 504, or 529 overloaded).
//
// Never a request the backup would refuse as well (any other 400, a context
// too long), a key refused (401, 403), or a rate limit (429): those fail as
// they would without a backup. Nor an error that isn't the API's (the
// caller gave up, the network).
func FallsBack(err error) bool {
	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	if CreditRefused(err) {
		return true
	}
	switch apiErr.StatusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

// CreditRefused says whether err is the Claude API refusing a turn for the
// balance of the account that pays for it: matched on all of its status
// (400), its error type (invalid_request_error) and its words ("credit
// balance").
func CreditRefused(err error) bool {
	var apiErr *sdk.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest &&
		string(apiErr.Type()) == "invalid_request_error" && strings.Contains(strings.ToLower(apiErr.Error()), "credit balance")
}

// toolsField is where the API's words for a refused request name one of its
// tools: "tools.3.custom.input_schema: …".
var toolsField = regexp.MustCompile(`\btools\.\d+\.`)

// ToolsRefused says whether err is the Claude API refusing a request for
// one of the tools it offered: matched on its status (400), its error type
// (invalid_request_error) and the field its words name (tools.N.…).
func ToolsRefused(err error) bool {
	var apiErr *sdk.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest &&
		string(apiErr.Type()) == "invalid_request_error" && toolsField.MatchString(apiErr.Error())
}
