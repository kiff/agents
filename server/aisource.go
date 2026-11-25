package server

import (
	"context"
	"net/http"
)

// AISource is where new runs' turns go, as the function is configured, so
// the apps can say on the AI consent which company runs the model (GET
// /ai/source). One place decides it, so the consent can't name another.
type AISource struct {
	// Source is where a new run's turns go: model.SourceAnthropic or
	// model.SourceBedrock.
	Source string `json:"source"`
	// Fallback, when set, is where a new run may go instead when Source
	// can't serve it: then the words may reach either.
	Fallback string `json:"fallback,omitempty"`
	// Geo is where Source runs the model, when it keeps to one place ("eu");
	// empty when it doesn't say.
	Geo string `json:"geo,omitempty"`
}

// getAISource answers where new runs' turns go. Unset, it's unavailable, so
// a caller shows nothing rather than a guess.
func (s *Server) getAISource(context.Context, Caller, *http.Request) (int, any, error) {
	if s.AISource.Source == "" {
		return 0, nil, unavailable("this server doesn't say where its turns run")
	}
	return http.StatusOK, s.AISource, nil
}
