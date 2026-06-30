package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-traker/internal/ipc"
)

const (
	statusInProgress        = "in_progress"
	statusNeedsConfirmation = "needs_confirmation"
	statusCompleted         = "completed"
)

type noteRecord struct {
	Text      string
	CreatedAt time.Time
}

type taskRecord struct {
	SessionID      string
	SessionName    string
	WindowID       string
	WindowName     string
	Pane           string
	Summary        string
	Notes          []noteRecord
	CWD            string
	Branch         string
	CompletionNote string
	StartedAt      time.Time
	CompletedAt    *time.Time
	Status         string
	Acknowledged   bool
}

type storedSettings struct {
	NotificationsEnabled *bool `json:"notifications_enabled,omitempty"`
}

type tmuxTarget struct {
	SessionName string
	SessionID   string
	WindowName  string
	WindowID    string
	PaneID      string
	WindowIndex string
	PaneIndex   string
}

type uiSubscriber struct {
	enc *json.Encoder
}

type server struct {
	mu                   sync.Mutex
	socketPath           string
	notificationsEnabled bool
	tasks                map[string]*taskRecord
	subscribers          map[*uiSubscriber]struct{}
	settingsPath         string
}

func newServer() *server {
	return &server{
		socketPath:           socketPath(),
		notificationsEnabled: true,
		tasks:                make(map[string]*taskRecord),
		subscribers:          make(map[*uiSubscriber]struct{}),
		settingsPath:         settingsStorePath(),
	}
}

func main() {
	log.SetFlags(log.LstdFlags)
	srv := newServer()
	if err := srv.run(); err != nil {
		log.Fatal(err)
	}
}

func (s *server) run() error {
	if err := s.loadSettings(); err != nil {
		log.Printf("load settings: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o755); err != nil {
		return err
	}
	_ = os.RemoveAll(s.socketPath)
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(s.socketPath)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				errCh <- err
				return
			}
			go s.handleConn(conn)
		}
	}()

	log.Printf("tracker-server listening on %s", s.socketPath)
	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		return fmt.Errorf("tracker-server stopped: %s", sig)
	}
}

func (s *server) handleConn(conn net.Conn) {
	defer conn.Close()

	dec := json.NewDecoder(bufio.NewReader(conn))
	enc := json.NewEncoder(conn)

	var sub *uiSubscriber
	defer func() {
		if sub != nil {
			s.removeSubscriber(sub)
		}
	}()

	for {
		var env ipc.Envelope
		if err := dec.Decode(&env); err != nil {
			return
		}
		switch env.Kind {
		case "command":
			if err := s.handleCommand(env); err != nil {
				log.Printf("command error: %v", err)
			}
			_ = enc.Encode(&ipc.Envelope{Kind: "ack"})
		case "ui-register":
			if sub == nil {
				sub = &uiSubscriber{enc: enc}
				s.addSubscriber(sub)
			}
			if err := s.sendStateTo(sub); err != nil {
				return
			}
		default:
			log.Printf("unknown message: %+v", env)
		}
	}
}

func (s *server) handleCommand(env ipc.Envelope) error {
	switch env.Command {
	case "start_task":
		target, err := requireSessionWindow(env)
		if err != nil {
			return err
		}
		summary := firstNonEmpty(env.Summary, env.Message)
		if summary == "" {
			return fmt.Errorf("start_task requires summary")
		}
		if err := s.startTask(target, summary, env.CWD, env.Branch); err != nil {
			return err
		}
		s.broadcastStateAsync()
		s.statusRefreshAsync()
		return nil
	case "finish_task":
		target, err := requireSessionWindow(env)
		if err != nil {
			return err
		}
		note := firstNonEmpty(env.Summary, env.Message)
		notify, err := s.finishTask(target, note, env.CWD, env.Branch)
		if err != nil {
			return err
		}
		if notify && s.notificationsAreEnabled() {
			go s.notifyResponded(target)
		}
		s.broadcastStateAsync()
		s.statusRefreshAsync()
		return nil
	case "notify":
		target, err := requireSessionWindow(env)
		if err != nil {
			return err
		}
		message := firstNonEmpty(env.Summary, env.Message)
		if message == "" {
			return fmt.Errorf("notify requires summary")
		}
		if s.notificationsAreEnabled() {
			return sendSystemNotification(notificationTitleForTarget(target), message, notificationActionForTarget(target))
		}
		return nil
	case "update_task":
		target, err := requireSessionWindow(env)
		if err != nil {
			return err
		}
		summary := firstNonEmpty(env.Summary, env.Message)
		if summary == "" {
			return fmt.Errorf("update_task requires summary")
		}
		if err := s.updateTaskSummary(target, summary, env.CWD, env.Branch); err != nil {
			return err
		}
		s.broadcastStateAsync()
		s.statusRefreshAsync()
		return nil
	case "needs_confirmation":
		target, err := requireSessionWindow(env)
		if err != nil {
			return err
		}
		summary := firstNonEmpty(env.Summary, env.Message)
		if err := s.markNeedsConfirmation(target, summary, env.CWD, env.Branch); err != nil {
			return err
		}
		s.broadcastStateAsync()
		s.statusRefreshAsync()
		return nil
	case "add_note":
		target, err := requireSessionWindow(env)
		if err != nil {
			return err
		}
		note := firstNonEmpty(env.Note, env.Summary, env.Message)
		if note == "" {
			return fmt.Errorf("add_note requires note")
		}
		if err := s.addNote(target, note, env.CWD, env.Branch); err != nil {
			return err
		}
		s.broadcastStateAsync()
		s.statusRefreshAsync()
		return nil
	case "delete_note":
		target, err := requireSessionWindow(env)
		if err != nil {
			return err
		}
		if err := s.deleteNote(target, env.NoteIndex); err != nil {
			return err
		}
		s.broadcastStateAsync()
		s.statusRefreshAsync()
		return nil
	case "acknowledge":
		target, err := requireSessionWindow(env)
		if err != nil {
			return err
		}
		if err := s.acknowledgeTask(target.SessionID, target.WindowID, target.PaneID); err != nil {
			return err
		}
		s.broadcastStateAsync()
		s.statusRefreshAsync()
		return nil
	case "delete_task":
		target, err := requireSessionWindow(env)
		if err != nil {
			return err
		}
		if err := s.deleteTask(target.SessionID, target.WindowID, target.PaneID); err != nil {
			return err
		}
		s.broadcastStateAsync()
		s.statusRefreshAsync()
		return nil
	default:
		return fmt.Errorf("unknown command %q", env.Command)
	}
}

func (s *server) startTask(target tmuxTarget, summary, cwd, branch string) error {
	if target.SessionID == "" || target.WindowID == "" {
		return fmt.Errorf("cannot create task: missing session or window ID")
	}
	target = normalizeTargetNames(target)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(target.SessionID, target.WindowID, target.PaneID)
	t, ok := s.tasks[key]
	if !ok {
		s.tasks[key] = &taskRecord{
			SessionID: target.SessionID, SessionName: strings.TrimSpace(target.SessionName),
			WindowID: target.WindowID, WindowName: strings.TrimSpace(target.WindowName),
			Pane: target.PaneID, Summary: summary, CWD: strings.TrimSpace(cwd), Branch: strings.TrimSpace(branch), StartedAt: now,
			Status: statusInProgress, Acknowledged: true,
		}
		return nil
	}
	mergeTaskNamesFromTarget(t, target)
	if !(t.Status == statusInProgress && strings.TrimSpace(t.Summary) != "") {
		t.Summary = summary
	}
	updateTaskContext(t, cwd, branch)
	t.StartedAt = now
	t.Status = statusInProgress
	t.CompletedAt = nil
	t.CompletionNote = ""
	t.Acknowledged = true
	return nil
}

func (s *server) updateTaskSummary(target tmuxTarget, summary, cwd, branch string) error {
	if target.SessionID == "" || target.WindowID == "" {
		return fmt.Errorf("cannot update task: missing session or window ID")
	}
	target = normalizeTargetNames(target)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(target.SessionID, target.WindowID, target.PaneID)
	t, ok := s.tasks[key]
	if !ok {
		t = &taskRecord{
			SessionID: target.SessionID, SessionName: strings.TrimSpace(target.SessionName),
			WindowID: target.WindowID, WindowName: strings.TrimSpace(target.WindowName),
			Pane: target.PaneID, CWD: strings.TrimSpace(cwd), Branch: strings.TrimSpace(branch), StartedAt: now, Status: statusInProgress, Acknowledged: true,
		}
		s.tasks[key] = t
	}
	mergeTaskNamesFromTarget(t, target)
	t.Summary = summary
	updateTaskContext(t, cwd, branch)
	if t.Status == "" {
		t.Status = statusInProgress
	}
	if t.StartedAt.IsZero() {
		t.StartedAt = now
	}
	return nil
}

func (s *server) markNeedsConfirmation(target tmuxTarget, summary, cwd, branch string) error {
	if target.SessionID == "" || target.WindowID == "" {
		return fmt.Errorf("cannot mark task: missing session or window ID")
	}
	target = normalizeTargetNames(target)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(target.SessionID, target.WindowID, target.PaneID)
	t, ok := s.tasks[key]
	if !ok {
		t = &taskRecord{
			SessionID: target.SessionID, SessionName: strings.TrimSpace(target.SessionName),
			WindowID: target.WindowID, WindowName: strings.TrimSpace(target.WindowName),
			Pane: target.PaneID, StartedAt: now, Status: statusNeedsConfirmation, Acknowledged: true,
		}
		s.tasks[key] = t
	}
	mergeTaskNamesFromTarget(t, target)
	if strings.TrimSpace(summary) != "" {
		t.Summary = strings.TrimSpace(summary)
	}
	if t.Summary == "" {
		t.Summary = "Needs user confirmation"
	}
	updateTaskContext(t, cwd, branch)
	t.Status = statusNeedsConfirmation
	if t.StartedAt.IsZero() {
		t.StartedAt = now
	}
	return nil
}

func (s *server) addNote(target tmuxTarget, text, cwd, branch string) error {
	if target.SessionID == "" || target.WindowID == "" {
		return fmt.Errorf("cannot add note: missing session or window ID")
	}
	target = normalizeTargetNames(target)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(target.SessionID, target.WindowID, target.PaneID)
	t, ok := s.tasks[key]
	if !ok {
		t = &taskRecord{
			SessionID: target.SessionID, SessionName: strings.TrimSpace(target.SessionName),
			WindowID: target.WindowID, WindowName: strings.TrimSpace(target.WindowName),
			Pane: target.PaneID, StartedAt: now, Status: statusInProgress, Acknowledged: true,
		}
		s.tasks[key] = t
	}
	mergeTaskNamesFromTarget(t, target)
	updateTaskContext(t, cwd, branch)
	t.Notes = append(t.Notes, noteRecord{Text: strings.TrimSpace(text), CreatedAt: now})
	return nil
}

func (s *server) deleteNote(target tmuxTarget, noteIndex *int) error {
	if target.SessionID == "" || target.WindowID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(target.SessionID, target.WindowID, target.PaneID)
	t, ok := s.tasks[key]
	if !ok || len(t.Notes) == 0 {
		return nil
	}
	// Default to the last note when no index is given (or it's out of range).
	idx := len(t.Notes) - 1
	if noteIndex != nil && *noteIndex >= 0 && *noteIndex < len(t.Notes) {
		idx = *noteIndex
	}
	t.Notes = append(t.Notes[:idx], t.Notes[idx+1:]...)
	return nil
}

func (s *server) finishTask(target tmuxTarget, note, cwd, branch string) (bool, error) {
	if target.SessionID == "" || target.WindowID == "" {
		return false, nil
	}
	target = normalizeTargetNames(target)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := taskKey(target.SessionID, target.WindowID, target.PaneID)
	t, ok := s.tasks[key]
	wasCompleted := false
	if !ok {
		t = &taskRecord{
			SessionID: target.SessionID, SessionName: strings.TrimSpace(target.SessionName),
			WindowID: target.WindowID, WindowName: strings.TrimSpace(target.WindowName),
			Pane: target.PaneID, StartedAt: now,
		}
		s.tasks[key] = t
	} else {
		wasCompleted = t.Status == statusCompleted
	}
	if t.Summary == "" {
		t.Summary = note
	}
	mergeTaskNamesFromTarget(t, target)
	updateTaskContext(t, cwd, branch)
	t.Status = statusCompleted
	t.CompletedAt = &now
	if note != "" {
		t.CompletionNote = note
	}
	t.Acknowledged = isActivePane(target.PaneID)
	return !wasCompleted, nil
}

func (s *server) acknowledgeTask(sessionID, windowID, paneID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[taskKey(sessionID, windowID, paneID)]; ok {
		t.Acknowledged = true
	}
	return nil
}

func (s *server) deleteTask(sessionID, windowID, paneID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskKey(sessionID, windowID, paneID))
	return nil
}

func normalizeTargetNames(target tmuxTarget) tmuxTarget {
	if strings.TrimSpace(target.SessionName) == strings.TrimSpace(target.SessionID) {
		target.SessionName = ""
	}
	if strings.TrimSpace(target.WindowName) == strings.TrimSpace(target.WindowID) {
		target.WindowName = ""
	}
	return target
}

func mergeTaskNamesFromTarget(task *taskRecord, target tmuxTarget) {
	if task == nil {
		return
	}
	if sn := strings.TrimSpace(target.SessionName); sn != "" {
		task.SessionName = sn
	}
	if wn := strings.TrimSpace(target.WindowName); wn != "" {
		task.WindowName = wn
	}
}

func updateTaskContext(task *taskRecord, cwd, branch string) {
	if task == nil {
		return
	}
	if v := strings.TrimSpace(cwd); v != "" {
		task.CWD = v
	}
	if v := strings.TrimSpace(branch); v != "" {
		task.Branch = v
	}
}

func (s *server) loadSettings() error {
	data, err := os.ReadFile(s.settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var stored storedSettings
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	if stored.NotificationsEnabled != nil {
		s.mu.Lock()
		s.notificationsEnabled = *stored.NotificationsEnabled
		s.mu.Unlock()
	}
	return nil
}

func (s *server) notificationsAreEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notificationsEnabled
}

func (s *server) notifyResponded(target tmuxTarget) {
	target = s.fillTargetNamesFromTask(target)
	summary := strings.TrimSpace(s.summaryForTask(target.SessionID, target.WindowID, target.PaneID))
	if summary == "" {
		summary = "Task marked complete"
	}
	if err := sendSystemNotification(notificationTitleForTarget(target), summary, notificationActionForTarget(target)); err != nil {
		log.Printf("notification error: %v", err)
	}
}

func (s *server) fillTargetNamesFromTask(target tmuxTarget) tmuxTarget {
	target = normalizeTargetNames(target)
	s.mu.Lock()
	defer s.mu.Unlock()
	if task, ok := s.tasks[taskKey(target.SessionID, target.WindowID, target.PaneID)]; ok {
		if strings.TrimSpace(target.SessionName) == "" {
			target.SessionName = strings.TrimSpace(task.SessionName)
		}
		if strings.TrimSpace(target.WindowName) == "" {
			target.WindowName = strings.TrimSpace(task.WindowName)
		}
	}
	return target
}

func notificationTitleForTarget(target tmuxTarget) string {
	target = normalizeTargetNames(target)
	session := strings.TrimSpace(target.SessionName)
	window := strings.TrimSpace(target.WindowName)
	if session == "" {
		session = strings.TrimSpace(target.SessionID)
	}
	if window == "" {
		window = strings.TrimSpace(target.WindowID)
	}
	if session != "" && window != "" {
		return session + " - " + window
	}
	if session != "" {
		return session
	}
	if window != "" {
		return window
	}
	return "Tracker"
}

func (s *server) summaryForTask(sessionID, windowID, paneID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[taskKey(sessionID, windowID, paneID)]; ok {
		note := strings.TrimSpace(t.CompletionNote)
		summary := strings.TrimSpace(t.Summary)
		if note != "" && !isGenericCompletionNote(note) {
			return note
		}
		if summary != "" {
			return summary
		}
		if note != "" {
			return note
		}
	}
	return ""
}

func isGenericCompletionNote(note string) bool {
	n := strings.ToLower(strings.TrimSpace(note))
	n = strings.Trim(n, ".!?,;:-_()[]{}\"'` ")
	switch n {
	case "", "done", "complete", "completed", "finished", "fixed", "resolved", "ok", "okay", "success", "implemented", "updated", "shipped":
		return true
	}
	return false
}

func (s *server) broadcastStateAsync() { go s.broadcastState() }

func (s *server) broadcastState() {
	env := s.buildStateEnvelope()
	if env == nil {
		return
	}
	s.mu.Lock()
	subs := make([]*uiSubscriber, 0, len(s.subscribers))
	for sub := range s.subscribers {
		subs = append(subs, sub)
	}
	s.mu.Unlock()
	for _, sub := range subs {
		if err := sub.enc.Encode(env); err != nil {
			s.removeSubscriber(sub)
		}
	}
}

func (s *server) statusRefreshAsync() {
	go func() {
		if err := runTmux("refresh-client", "-S"); err != nil {
			log.Printf("status refresh error: %v", err)
		}
	}()
}

func (s *server) sendStateTo(sub *uiSubscriber) error {
	env := s.buildStateEnvelope()
	if env == nil {
		return nil
	}
	if err := sub.enc.Encode(env); err != nil {
		s.removeSubscriber(sub)
		return err
	}
	return nil
}

func (s *server) buildStateEnvelope() *ipc.Envelope {
	s.mu.Lock()
	copies := make([]*taskRecord, 0, len(s.tasks))
	for _, task := range s.tasks {
		c := *task
		copies = append(copies, &c)
	}
	s.mu.Unlock()

	now := time.Now()
	tasks := make([]ipc.Task, 0, len(copies))
	nameCache := make(map[string][2]string)
	for _, t := range copies {
		started := ""
		if !t.StartedAt.IsZero() {
			started = t.StartedAt.Format(time.RFC3339)
		}
		completed := ""
		var duration time.Duration
		if t.CompletedAt != nil {
			completed = t.CompletedAt.Format(time.RFC3339)
			duration = t.CompletedAt.Sub(t.StartedAt)
		} else {
			duration = now.Sub(t.StartedAt)
		}
		if duration < 0 {
			duration = 0
		}
		sessionName := strings.TrimSpace(t.SessionName)
		windowName := strings.TrimSpace(t.WindowName)
		if sessionName == strings.TrimSpace(t.SessionID) {
			sessionName = ""
		}
		if windowName == strings.TrimSpace(t.WindowID) {
			windowName = ""
		}
		if sessionName == "" || windowName == "" {
			if cached, ok := nameCache[t.WindowID]; ok {
				if sessionName == "" {
					sessionName = cached[0]
				}
				if windowName == "" {
					windowName = cached[1]
				}
			} else {
				sn, wn, err := tmuxNamesForWindow(t.WindowID)
				if err == nil {
					nameCache[t.WindowID] = [2]string{sn, wn}
					if sessionName == "" {
						sessionName = sn
					}
					if windowName == "" {
						windowName = wn
					}
				}
			}
		}
		if sessionName == "" {
			sessionName = t.SessionID
		}
		if windowName == "" {
			windowName = t.WindowID
		}
		notes := make([]ipc.Note, 0, len(t.Notes))
		for _, note := range t.Notes {
			text := strings.TrimSpace(note.Text)
			if text == "" {
				continue
			}
			created := ""
			if !note.CreatedAt.IsZero() {
				created = note.CreatedAt.Format(time.RFC3339)
			}
			notes = append(notes, ipc.Note{Text: text, CreatedAt: created})
		}
		tasks = append(tasks, ipc.Task{
			SessionID: t.SessionID, Session: sessionName, WindowID: t.WindowID,
			Window: windowName, Pane: t.Pane, Status: t.Status, Summary: t.Summary, Notes: notes,
			CWD: strings.TrimSpace(t.CWD), Branch: strings.TrimSpace(t.Branch),
			CompletionNote: t.CompletionNote, StartedAt: started, CompletedAt: completed,
			DurationSeconds: duration.Seconds(), Acknowledged: t.Acknowledged,
		})
	}
	return &ipc.Envelope{Kind: "state", Message: stateSummary(tasks), Tasks: tasks}
}

func (s *server) addSubscriber(sub *uiSubscriber) {
	s.mu.Lock()
	s.subscribers[sub] = struct{}{}
	s.mu.Unlock()
}

func (s *server) removeSubscriber(sub *uiSubscriber) {
	s.mu.Lock()
	delete(s.subscribers, sub)
	s.mu.Unlock()
}

type notificationAction struct {
	Command     string
	ActivateApp string
}

func notificationActionForTarget(target tmuxTarget) *notificationAction {
	session := strings.TrimSpace(target.SessionID)
	window := strings.TrimSpace(target.WindowID)
	pane := strings.TrimSpace(target.PaneID)
	if session == "" || window == "" || pane == "" {
		return nil
	}
	cmd := fmt.Sprintf("tmux switch-client -t %s && tmux select-window -t %s && tmux select-pane -t %s",
		shellQuote(session), shellQuote(window), shellQuote(pane))
	return &notificationAction{Command: "sh -lc " + strconv.Quote(cmd), ActivateApp: "com.googlecode.iterm2"}
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func sendSystemNotification(title, message string, action *notificationAction) error {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Tracker"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = title
	}
	switch runtime.GOOS {
	case "darwin":
		if bin, err := exec.LookPath("terminal-notifier"); err == nil {
			args := []string{"-title", title, "-message", message, "-group", "agent-tracker"}
			if action != nil {
				if strings.TrimSpace(action.Command) != "" {
					args = append(args, "-execute", action.Command)
				}
				if strings.TrimSpace(action.ActivateApp) != "" {
					args = append(args, "-activate", action.ActivateApp)
				}
			}
			return exec.Command(bin, args...).Run()
		}
		script := fmt.Sprintf("display notification %s with title %s", strconv.Quote(message), strconv.Quote(title))
		return exec.Command("osascript", "-e", script).Run()
	case "linux":
		if _, err := exec.LookPath("notify-send"); err != nil {
			return err
		}
		return exec.Command("notify-send", title, message).Run()
	}
	return nil
}

func runTmux(args ...string) error {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			return fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, trimmed)
		}
		return fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func isActivePane(paneID string) bool {
	clients, err := listClients()
	if err != nil {
		return false
	}
	for _, client := range clients {
		out, err := tmuxDisplay(client, "#{pane_id}")
		if err != nil {
			continue
		}
		if strings.TrimSpace(out) == paneID {
			return true
		}
	}
	return false
}

func tmuxDisplay(client, format string) (string, error) {
	out, err := exec.Command("tmux", "display-message", "-p", "-c", client, format).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("display-message %s: %w (%s)", format, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func listClients() ([]string, error) {
	out, err := exec.Command("tmux", "list-clients", "-F", "#{client_tty}").CombinedOutput()
	if err != nil {
		return nil, err
	}
	var clients []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			clients = append(clients, t)
		}
	}
	return clients, nil
}

func socketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "agent-tracker.sock")
	}
	return filepath.Join(os.TempDir(), "agent-tracker.sock")
}

func settingsStorePath() string {
	return filepath.Join(os.Getenv("HOME"), ".config", "agent-tracker", "run", "settings.json")
}

func taskKey(sessionID, windowID, paneID string) string {
	return strings.Join([]string{sessionID, windowID, paneID}, "|")
}

func requireSessionWindow(env ipc.Envelope) (tmuxTarget, error) {
	ctx := normalizeTargetNames(tmuxTarget{
		SessionName: strings.TrimSpace(env.Session),
		SessionID:   strings.TrimSpace(env.SessionID),
		WindowName:  strings.TrimSpace(env.Window),
		WindowID:    strings.TrimSpace(env.WindowID),
		PaneID:      strings.TrimSpace(env.Pane),
	})
	fetchOrder := []string{}
	if ctx.PaneID != "" {
		fetchOrder = append(fetchOrder, ctx.PaneID)
	}
	if ctx.WindowID != "" {
		fetchOrder = append(fetchOrder, ctx.WindowID)
	}
	fetchOrder = append(fetchOrder, "")
	for _, target := range fetchOrder {
		if ctx.complete() {
			break
		}
		info, err := detectTmuxTarget(target)
		if err != nil {
			if target == "" {
				return tmuxTarget{}, err
			}
			continue
		}
		ctx = ctx.merge(info)
	}
	if ctx.SessionID == "" || ctx.WindowID == "" {
		return tmuxTarget{}, fmt.Errorf("session and window required")
	}
	if ctx.SessionName == "" || ctx.WindowName == "" {
		if info, err := detectTmuxTarget(ctx.WindowID); err == nil {
			ctx = ctx.merge(normalizeTargetNames(info))
		}
	}
	if ctx.SessionName == "" {
		ctx.SessionName = ctx.SessionID
	}
	if ctx.WindowName == "" {
		ctx.WindowName = ctx.WindowID
	}
	if strings.TrimSpace(ctx.PaneID) == "" {
		return tmuxTarget{}, fmt.Errorf("pane identifier required")
	}
	return ctx, nil
}

func (t tmuxTarget) complete() bool {
	return t.SessionName != "" && t.SessionID != "" && t.WindowName != "" && t.WindowID != "" && t.PaneID != ""
}

func (t tmuxTarget) merge(other tmuxTarget) tmuxTarget {
	if t.SessionName == "" {
		t.SessionName = other.SessionName
	}
	if t.SessionID == "" {
		t.SessionID = other.SessionID
	}
	if t.WindowName == "" {
		t.WindowName = other.WindowName
	}
	if t.WindowID == "" {
		t.WindowID = other.WindowID
	}
	if t.PaneID == "" {
		t.PaneID = other.PaneID
	}
	return t
}

func detectTmuxTarget(target string) (tmuxTarget, error) {
	format := "#{session_name}:::#{session_id}:::#{window_name}:::#{window_id}:::#{pane_id}:::#{window_index}:::#{pane_index}"
	out, err := tmuxQuery(strings.TrimSpace(target), format)
	if err != nil {
		return tmuxTarget{}, err
	}
	parts := strings.Split(strings.TrimSpace(out), ":::")
	if len(parts) != 7 {
		return tmuxTarget{}, fmt.Errorf("unexpected tmux response: %s", strings.TrimSpace(out))
	}
	return tmuxTarget{
		SessionName: strings.TrimSpace(parts[0]), SessionID: strings.TrimSpace(parts[1]),
		WindowName: strings.TrimSpace(parts[2]), WindowID: strings.TrimSpace(parts[3]),
		PaneID: strings.TrimSpace(parts[4]), WindowIndex: strings.TrimSpace(parts[5]),
		PaneIndex: strings.TrimSpace(parts[6]),
	}, nil
}

func tmuxNamesForWindow(windowID string) (string, string, error) {
	if strings.TrimSpace(windowID) == "" {
		return "", "", fmt.Errorf("window id required")
	}
	out, err := tmuxQuery(strings.TrimSpace(windowID), "#{session_name}:::#{window_name}")
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.TrimSpace(out), ":::")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected tmux response: %s", strings.TrimSpace(out))
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func tmuxQuery(target, format string) (string, error) {
	args := []string{"display-message", "-p"}
	if target != "" {
		args = append(args, "-t", target)
	}
	args = append(args, format)
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func stateSummary(tasks []ipc.Task) string {
	inProgress, confirm, waiting := 0, 0, 0
	for _, t := range tasks {
		switch t.Status {
		case statusInProgress:
			inProgress++
		case statusNeedsConfirmation:
			confirm++
		case statusCompleted:
			if !t.Acknowledged {
				waiting++
			}
		}
	}
	return fmt.Sprintf("Active %d · Confirm %d · Waiting %d · %s", inProgress, confirm, waiting, time.Now().Format(time.Kitchen))
}
