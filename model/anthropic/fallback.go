package anthropic

import (
	"errors"
	"net/http"
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
	switch apiErr.StatusCode {
	case http.StatusBadRequest:
		return string(apiErr.Type()) == "invalid_request_error" && strings.Contains(strings.ToLower(apiErr.Error()), "credit balance")
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}
