package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agent-traker/internal/ipc"
)

const (
	implementationName    = "tracker_mcp"
	implementationVersion = "0.1.0"
	commandTimeout        = 5 * time.Second
)

type trackerClient struct {
	socket string
}

func newTrackerClient() *trackerClient {
	socket := strings.TrimSpace(os.Getenv("TRACKER_SOCKET"))
	if socket == "" {
		socket = socketPath()
	}
	return &trackerClient{socket: socket}
}

func (c *trackerClient) sendCommand(ctx context.Context, env ipc.Envelope) error {
	env.Kind = "command"
	d := net.Dialer{}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, commandTimeout)
		defer cancel()
	}
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(&env); err != nil {
		return err
	}
	for {
		var reply ipc.Envelope
		if err := dec.Decode(&reply); err != nil {
			return err
		}
		if reply.Kind == "ack" {
			return nil
		}
	}
}

type startInput struct {
	Summary string `json:"summary"`
	TmuxID  string `json:"tmux_id"`
	CWD     string `json:"cwd,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

type finishInput struct {
	Note   *string `json:"note,omitempty"`
	TmuxID string  `json:"tmux_id"`
	CWD    string  `json:"cwd,omitempty"`
	Branch string  `json:"branch,omitempty"`
}

type updateInput struct {
	Summary string `json:"summary"`
	TmuxID  string `json:"tmux_id"`
	CWD     string `json:"cwd,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

func main() {
	log.SetFlags(0)
	client := newTrackerClient()

	server := mcp.NewServer(&mcp.Implementation{Name: implementationName, Version: implementationVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "start_working",
		Description: "Record that work has started for a tmux session/window/pane. Pass tmux_id as session_id::window_id::pane_id (e.g. $3::@12::%30) and a short summary of the task.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input startInput) (*mcp.CallToolResult, any, error) {
		target, err := parseTmuxID(input.TmuxID)
		if err != nil {
			return nil, nil, err
		}
		summary := strings.TrimSpace(input.Summary)
		if summary == "" {
			return nil, nil, fmt.Errorf("summary is required")
		}
		if err := client.sendCommand(ctx, ipc.Envelope{
			Command: "start_task", SessionID: target.SessionID, WindowID: target.WindowID,
			Pane: target.PaneID, Summary: summary, CWD: input.CWD, Branch: input.Branch,
		}); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Task started."}}}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "finish_working",
		Description: "Mark the task for a tmux session/window/pane as completed. Pass tmux_id and an optional completion note.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input finishInput) (*mcp.CallToolResult, any, error) {
		target, err := parseTmuxID(input.TmuxID)
		if err != nil {
			return nil, nil, err
		}
		env := ipc.Envelope{Command: "finish_task", SessionID: target.SessionID, WindowID: target.WindowID, Pane: target.PaneID, CWD: input.CWD, Branch: input.Branch}
		if input.Note != nil && strings.TrimSpace(*input.Note) != "" {
			note := strings.TrimSpace(*input.Note)
			env.Summary = note
		}
		if err := client.sendCommand(ctx, env); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Task finished."}}}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_summary",
		Description: "Update the summary of an in-progress task for a tmux session/window/pane.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input updateInput) (*mcp.CallToolResult, any, error) {
		target, err := parseTmuxID(input.TmuxID)
		if err != nil {
			return nil, nil, err
		}
		summary := strings.TrimSpace(input.Summary)
		if summary == "" {
			return nil, nil, fmt.Errorf("summary is required")
		}
		if err := client.sendCommand(ctx, ipc.Envelope{
			Command: "update_task", SessionID: target.SessionID, WindowID: target.WindowID,
			Pane: target.PaneID, Summary: summary, CWD: input.CWD, Branch: input.Branch,
		}); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Task updated."}}}, nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

type tmuxContext struct {
	SessionID string
	WindowID  string
	PaneID    string
}

func parseTmuxID(tmuxID string) (tmuxContext, error) {
	parts := strings.Split(strings.TrimSpace(tmuxID), "::")
	if len(parts) != 3 {
		return tmuxContext{}, fmt.Errorf("tmux_id must be session_id::window_id::pane_id")
	}
	sessionID := strings.TrimSpace(parts[0])
	windowID := strings.TrimSpace(parts[1])
	paneID := strings.TrimSpace(parts[2])
	if sessionID == "" || windowID == "" || paneID == "" {
		return tmuxContext{}, fmt.Errorf("tmux_id must include non-empty session, window, and pane identifiers")
	}
	return tmuxContext{SessionID: sessionID, WindowID: windowID, PaneID: paneID}, nil
}

func socketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "agent-tracker.sock")
	}
	return filepath.Join(os.TempDir(), "agent-tracker.sock")
}
