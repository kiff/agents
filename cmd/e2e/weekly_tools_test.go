package main

import (
	"context"
	"strings"
	"testing"
)

type fakeTafTools []string

func (f fakeTafTools) TafTools(context.Context, string) ([]string, error) { return f, nil }

func TestTheTafGainsTheApprovedConnectionsTools(t *testing.T) {
	conn := []string{"pipedrive_list_persons_x9k2", "pipedrive_add_activity_x9k2"}
	if err := toolsGained(context.Background(), fakeTafTools{"gmail_send_ab12", "pipedrive_list_persons_x9k2", "pipedrive_add_activity_x9k2"}, "agt", conn); err != nil {
		t.Fatal(err)
	}
	err := toolsGained(context.Background(), fakeTafTools{"pipedrive_list_persons_x9k2"}, "agt", conn)
	if err == nil || !strings.Contains(err.Error(), "pipedrive_add_activity_x9k2") {
		t.Fatalf("a missing tool: %v", err)
	}
}
