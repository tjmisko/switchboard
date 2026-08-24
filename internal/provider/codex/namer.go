package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

const DefaultDisplayNameModel = "gpt-5.6-luna"

// NamingContext is the bounded, ephemeral context for one completed Codex
// turn. Callers must bound both content fields before constructing it; no
// implementation may persist or log them.
type NamingContext struct {
	CWDBase           string
	UserPrompt        string
	AssistantResponse string
}

type NameGenerator interface {
	Generate(context.Context, NamingContext, string) (string, error)
}

// EphemeralNamer creates one isolated, non-persisted app-server thread. It
// captures only the final agent message and never logs the naming prompt.
type EphemeralNamer struct {
	Connector Connector
}

func (n EphemeralNamer) Generate(ctx context.Context, input NamingContext, model string) (string, error) {
	connector := n.Connector
	if connector == nil {
		connector = StdioServerConnector{}
	}
	connection, err := connector.Connect(ctx)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	message := make(chan string, 1)
	completed := make(chan struct{}, 1)
	client := newRPCClientWithRequests(connection, 1, func(notification rpcNotification) {
		switch notification.Method {
		case "item/completed":
			var params struct {
				Item struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(notification.Params, &params) == nil && params.Item.Type == "agentMessage" {
				select {
				case message <- params.Item.Text:
				default:
				}
			}
		case "turn/completed":
			select {
			case completed <- struct{}{}:
			default:
			}
		}
	}, displayNameRequests)
	defer client.Close()
	var initialized initializeResult
	if err := client.request(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "switchboard_display_name", "title": "Switchboard Display Name", "version": "1"},
	}, &initialized); err != nil {
		return "", err
	}
	if err := checkAppServerVersion(initialized.UserAgent); err != nil {
		return "", err
	}
	if err := client.notify("initialized", map[string]any{}); err != nil {
		return "", err
	}
	if model == "" {
		model = DefaultDisplayNameModel
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := client.request(ctx, "thread/start", map[string]any{
		"model": model, "ephemeral": true, "approvalPolicy": "never", "sandbox": "read-only",
	}, &started); err != nil {
		return "", err
	}
	if started.Thread.ID == "" {
		return "", errors.New("display-name app-server returned no thread")
	}
	prompt := "Create a lowercase 2-5 word kebab-case title, at most 40 characters, for this completed coding turn. Return only the title.\n" +
		"project: " + input.CWDBase + "\nuser request: " + input.UserPrompt + "\nfinal response: " + input.AssistantResponse
	if err := client.request(ctx, "turn/start", map[string]any{
		"threadId": started.Thread.ID,
		"input":    []map[string]string{{"type": "text", "text": prompt}},
	}, &struct{}{}); err != nil {
		return "", err
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-client.Done():
			return "", ErrDisconnected
		case <-completed:
			select {
			case value := <-message:
				return strings.TrimSpace(value), nil
			default:
				return "", errors.New("display-name turn completed without a name")
			}
		}
	}
}
