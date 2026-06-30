package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agent-traker/internal/ipc"
)

const (
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
)

var (
	styleTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	styleMeta    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleMuted   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleLive    = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	styleReview  = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	styleDone    = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	styleError   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleSelBG   = lipgloss.Color("236")
)

type keyConfig struct {
	Up, Down, Open, Cancel, Toggle, Delete, Help, Refresh string
}

func defaultKeys() keyConfig {
	return keyConfig{Up: "k", Down: "j", Open: "enter", Cancel: "esc",
		Toggle: "c", Delete: "D", Help: "?", Refresh: "r"}
}

type rawConfig struct {
	Keys map[string]string `json:"keys"`
}

func loadKeys() keyConfig {
	k := defaultKeys()
	paths := []string{
		filepath.Join(os.Getenv("HOME"), ".config", "agent-tracker", "agent-config.json"),
		filepath.Join(os.Getenv("HOME"), ".config", "agent", "agent-config.json"),
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var raw rawConfig
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		apply := func(field string, val string) {
			v := strings.TrimSpace(val)
			if v == "" {
				return
			}
			switch field {
			case "move_up":
				k.Up = v
			case "move_down":
				k.Down = v
			case "edit":
				k.Open = v
			case "cancel":
				k.Cancel = v
			case "toggle_todo":
				k.Toggle = v
			case "destroy":
				k.Delete = v
			case "help":
				k.Help = v
			}
		}
		for name, val := range raw.Keys {
			apply(name, val)
		}
		break
	}
	return k
}

type animateMsg time.Time
type refreshMsg time.Time
type stateMsg struct{ env *ipc.Envelope; err error }
type cmdResultMsg struct{ err error }

type model struct {
	keys    keyConfig
	width   int
	height  int
	state   ipc.Envelope
	cursor  int
	err     string
	help    bool
	ctx     tmuxCtx
	loaded  bool
}

type tmuxCtx struct {
	SessionName, SessionID, WindowName, WindowID, PaneID string
}

func main() {
	oneShot := flag.Bool("once", false, "print current state once and exit (non-interactive)")
	flag.Parse()

	if *oneShot {
		env, err := loadState()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println(renderOneShot(env))
		return
	}

	keys := loadKeys()
	m := model{keys: keys}
	m.refreshContext()
	p := tea.NewProgram(&m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(animate(), refresh(), poll())
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case animateMsg:
		return m, animate()
	case refreshMsg:
		return m, tea.Batch(refresh(), poll())
	case stateMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.err = ""
		m.state = *msg.env
		m.loaded = true
		m.refreshContext()
		m.clamp()
		return m, nil
	case cmdResultMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		}
		return m, poll()
	case tea.KeyMsg:
		return m.handleKey(msg.String())
	}
	return m, nil
}

func (m *model) handleKey(key string) (tea.Model, tea.Cmd) {
	if key == m.keys.Cancel || key == "q" || key == "ctrl+c" || key == "alt+a" || key == "M-a" || key == "meta+a" {
		return m, tea.Quit
	}
	if key == m.keys.Help {
		m.help = !m.help
		return m, nil
	}
	if m.help {
		return m, nil
	}
	switch key {
	case m.keys.Up, "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case m.keys.Down, "down":
		if m.cursor < len(m.tasks())-1 {
			m.cursor++
		}
	case m.keys.Open:
		return m, m.focusSelected()
	case m.keys.Toggle:
		return m, m.toggleSelected()
	case m.keys.Delete:
		return m, m.deleteSelected()
	case m.keys.Refresh, "r":
		return m, poll()
	}
	return m, nil
}

func (m *model) tasks() []ipc.Task {
	tasks := append([]ipc.Task(nil), m.state.Tasks...)
	sortTasks(tasks)
	return tasks
}

func (m *model) selected() *ipc.Task {
	tasks := m.tasks()
	if m.cursor < 0 || m.cursor >= len(tasks) {
		return nil
	}
	t := tasks[m.cursor]
	return &t
}

func (m *model) clamp() {
	if n := len(m.tasks()); m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *model) focusSelected() tea.Cmd {
	t := m.selected()
	if t == nil {
		return nil
	}
	return func() tea.Msg {
		err := focusTask(*t)
		return cmdResultMsg{err: err}
	}
}

func (m *model) toggleSelected() tea.Cmd {
	t := m.selected()
	if t == nil {
		return nil
	}
	task := *t
	return func() tea.Msg {
		env := ipc.Envelope{SessionID: task.SessionID, WindowID: task.WindowID, Pane: task.Pane}
		cmd := "acknowledge"
		if task.Status == statusInProgress {
			cmd = "finish_task"
		}
		return cmdResultMsg{err: sendCommand(cmd, &env)}
	}
}

func (m *model) deleteSelected() tea.Cmd {
	t := m.selected()
	if t == nil {
		return nil
	}
	task := *t
	return func() tea.Msg {
		env := ipc.Envelope{SessionID: task.SessionID, WindowID: task.WindowID, Pane: task.Pane}
		return cmdResultMsg{err: sendCommand("delete_task", &env)}
	}
}

func (m *model) refreshContext() {
	ctx := tmuxCtx{}
	out, err := tmuxOutput("display-message", "-p", "#{session_name}:::#{session_id}:::#{window_name}:::#{window_id}:::#{pane_id}")
	if err == nil {
		parts := strings.Split(strings.TrimSpace(out), ":::")
		if len(parts) == 5 {
			ctx.SessionName = parts[0]
			ctx.SessionID = parts[1]
			ctx.WindowName = parts[2]
			ctx.WindowID = parts[3]
			ctx.PaneID = parts[4]
		}
	}
	m.ctx = ctx
}

func (m model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	width := m.width
	var b strings.Builder
	b.WriteString(styleTitle.Render("Tracker"))
	if loc := m.contextLine(); loc != "" {
		b.WriteString(styleMeta.Render("  ·  " + loc))
	}
	b.WriteString("\n")
	b.WriteString(styleMeta.Render(m.metricsLine()))
	b.WriteString("\n\n")

	if m.help {
		b.WriteString(m.renderHelp(width))
		b.WriteString("\n")
	} else {
		b.WriteString(m.renderTasks(width))
	}
	if m.err != "" {
		b.WriteString("\n")
		b.WriteString(styleError.Render("error: " + m.err))
	}
	b.WriteString("\n")
	b.WriteString(styleMeta.Render(m.footer()))
	return b.String()
}

func (m model) contextLine() string {
	parts := []string{}
	if strings.TrimSpace(m.ctx.SessionName) != "" {
		parts = append(parts, m.ctx.SessionName)
	}
	if strings.TrimSpace(m.ctx.WindowName) != "" {
		parts = append(parts, m.ctx.WindowName)
	}
	return strings.Join(parts, "  ·  ")
}

func (m model) metricsLine() string {
	if msg := strings.TrimSpace(m.state.Message); msg != "" {
		return msg
	}
	active, review := 0, 0
	for _, t := range m.state.Tasks {
		switch t.Status {
		case statusInProgress:
			active++
		case statusCompleted:
			if !t.Acknowledged {
				review++
			}
		}
	}
	return fmt.Sprintf("%d live  ·  %d review", active, review)
}

func (m model) renderTasks(width int) string {
	tasks := m.tasks()
	if len(tasks) == 0 {
		return styleMuted.Render("No tasks in motion. Agents report tasks via the tracker MCP.")
	}
	now := time.Now()
	rowsPerPage := m.height - 6
	if rowsPerPage < 1 {
		rowsPerPage = 1
	}
	start := m.cursor
	if start > len(tasks)-rowsPerPage && len(tasks) >= rowsPerPage {
		start = len(tasks) - rowsPerPage
	}
	if start < 0 {
		start = 0
	}
	end := start + rowsPerPage
	if end > len(tasks) {
		end = len(tasks)
	}
	var lines []string
	for i := start; i < end; i++ {
		lines = append(lines, m.renderRow(tasks[i], i == m.cursor, width, now))
	}
	return strings.Join(lines, "\n")
}

func (m model) renderRow(t ipc.Task, selected bool, width int, now time.Time) string {
	indicator := taskIndicator(t, now)
	indicatorStyle := styleLive
	titleStyle := lipgloss.NewStyle().Bold(true)
	metaStyle := styleMeta
	switch t.Status {
	case statusCompleted:
		if t.Acknowledged {
			indicatorStyle = styleDone
			titleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
		} else {
			indicatorStyle = styleReview
			titleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("230"))
		}
	}
	if selected {
		bg := styleSelBG
		indicatorStyle = indicatorStyle.Background(bg)
		titleStyle = titleStyle.Background(bg).Foreground(lipgloss.Color("230"))
		metaStyle = metaStyle.Background(bg).Foreground(lipgloss.Color("251"))
	}
	title := fit(strings.TrimSpace(t.Summary), width-4)
	meta := strings.TrimSpace(t.Session)
	if w := strings.TrimSpace(t.Window); w != "" {
		meta += "  /  " + w
	}
	if cwd := displayCWD(t.CWD); cwd != "" {
		meta += "  ·  " + cwd
	}
	if branch := strings.TrimSpace(t.Branch); branch != "" {
		meta += "   " + branch
	}
	if t.Status == statusCompleted && !t.Acknowledged {
		meta += "  ·  awaiting review"
	}
	if d := liveDuration(t, now); d != "" {
		meta = strings.TrimSpace(meta + "  ·  " + d)
	}
	meta = fit(meta, width-2)
	return " " + indicatorStyle.Render(indicator) + " " + titleStyle.Render(title) + "\n   " + metaStyle.Render(meta)
}

func (m model) renderHelp(width int) string {
	lines := []string{
		fmt.Sprintf("%s / %s   move up / down", m.keys.Up, m.keys.Down),
		fmt.Sprintf("%s          open the task's tmux pane", m.keys.Open),
		fmt.Sprintf("%s          finish an in-progress task / acknowledge a completed one", m.keys.Toggle),
		fmt.Sprintf("%s          delete the selected task", m.keys.Delete),
		fmt.Sprintf("%s          refresh now", m.keys.Refresh),
		fmt.Sprintf("%s          toggle this help", m.keys.Help),
		fmt.Sprintf("%s / q      quit", m.keys.Cancel),
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = styleMuted.Render(fit(l, width-2))
	}
	return strings.Join(out, "\n")
}

func (m model) footer() string {
	return fmt.Sprintf("%s/%s move  %s open  %s toggle  %s delete  %s refresh  %s help  %s quit",
		m.keys.Up, m.keys.Down, m.keys.Open, m.keys.Toggle, m.keys.Delete, m.keys.Refresh, m.keys.Help, m.keys.Cancel)
}

func displayCWD(path string) string {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return ""
	}
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return path
	}
	parent := filepath.Base(filepath.Dir(path))
	if parent == "." || parent == string(filepath.Separator) || parent == "" {
		return base
	}
	return parent + "/" + base
}

func taskIndicator(t ipc.Task, now time.Time) string {
	switch t.Status {
	case statusInProgress:
		frames := []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}
		return string(frames[int(now.UnixNano()/int64(100*time.Millisecond))%len(frames)])
	case statusCompleted:
		if t.Acknowledged {
			return "✓"
		}
		return "⚑"
	}
	return "•"
}

func liveDuration(t ipc.Task, now time.Time) string {
	start, ok := parseTime(t.StartedAt)
	if !ok {
		return formatDuration(t.DurationSeconds)
	}
	if t.Status == statusCompleted {
		if end, ok := parseTime(t.CompletedAt); ok {
			return formatDuration(end.Sub(start).Seconds())
		}
		return formatDuration(t.DurationSeconds)
	}
	return formatDuration(now.Sub(start).Seconds())
}

func formatDuration(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	d := time.Duration(seconds * float64(time.Second))
	if d >= 99*time.Hour {
		return ">=99h"
	}
	h := d / time.Hour
	mm := (d % time.Hour) / time.Minute
	ss := (d % time.Minute) / time.Second
	if h > 0 {
		return fmt.Sprintf("%02dh%02dm", h, mm)
	}
	if mm > 0 {
		return fmt.Sprintf("%02dm%02ds", mm, ss)
	}
	return fmt.Sprintf("%02ds", ss)
}

func parseTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	return t, err == nil
}

func sortTasks(tasks []ipc.Task) {
	sort.SliceStable(tasks, func(i, j int) bool {
		a, b := tasks[i], tasks[j]
		ra, rb := statusRank(a.Status), statusRank(b.Status)
		if ra != rb {
			return ra < rb
		}
		if a.Status == statusInProgress {
			return a.StartedAt < b.StartedAt
		}
		return a.CompletedAt > b.CompletedAt
	})
}

func statusRank(s string) int {
	switch s {
	case statusInProgress:
		return 0
	case statusCompleted:
		return 1
	}
	return 2
}

func fit(s string, w int) string {
	disp := lipgloss.Width(s)
	if disp <= w {
		return s + strings.Repeat(" ", w-disp)
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)) > w-1 {
		r = r[:len(r)-1]
	}
	if w >= 1 {
		return string(r) + "…"
	}
	return ""
}

func animate() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return animateMsg(t) })
}

func refresh() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return refreshMsg(t) })
}

func poll() tea.Cmd {
	return func() tea.Msg {
		env, err := loadState()
		return stateMsg{env: env, err: err}
	}
}

func socketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "agent-tracker.sock")
	}
	return filepath.Join(os.TempDir(), "agent-tracker.sock")
}

func loadState() (*ipc.Envelope, error) {
	conn, err := net.DialTimeout("unix", socketPath(), time.Second)
	if err != nil {
		return nil, fmt.Errorf("tracker-server not running: %w", err)
	}
	defer conn.Close()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(bufio.NewReader(conn))
	if err := enc.Encode(&ipc.Envelope{Kind: "ui-register"}); err != nil {
		return nil, err
	}
	for {
		var env ipc.Envelope
		if err := dec.Decode(&env); err != nil {
			return nil, err
		}
		if env.Kind == "state" {
			return &env, nil
		}
	}
}

func sendCommand(command string, env *ipc.Envelope) error {
	conn, err := net.DialTimeout("unix", socketPath(), time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	req := ipc.Envelope{Kind: "command", Command: command}
	if env != nil {
		req.SessionID = env.SessionID
		req.WindowID = env.WindowID
		req.Pane = env.Pane
		req.Summary = env.Summary
	}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		return err
	}
	dec := json.NewDecoder(bufio.NewReader(conn))
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

func focusTask(t ipc.Task) error {
	if strings.TrimSpace(t.SessionID) == "" {
		return fmt.Errorf("session required to focus task")
	}
	if err := runTmux("switch-client", "-t", strings.TrimSpace(t.SessionID)); err != nil {
		return err
	}
	if strings.TrimSpace(t.WindowID) != "" {
		if err := runTmux("select-window", "-t", strings.TrimSpace(t.WindowID)); err != nil {
			return err
		}
	}
	if strings.TrimSpace(t.Pane) != "" {
		if err := runTmux("select-pane", "-t", strings.TrimSpace(t.Pane)); err != nil {
			return err
		}
	}
	return nil
}

func runTmux(args ...string) error {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func tmuxOutput(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func renderOneShot(env *ipc.Envelope) string {
	if env == nil {
		return "no state"
	}
	tasks := append([]ipc.Task(nil), env.Tasks...)
	sortTasks(tasks)
	var b strings.Builder
	fmt.Fprintln(&b, env.Message)
	for _, t := range tasks {
		mark := "•"
		switch t.Status {
		case statusInProgress:
			mark = "▶"
		case statusCompleted:
			if t.Acknowledged {
				mark = "✓"
			} else {
				mark = "⚑"
			}
		}
		meta := strings.TrimSpace(t.Session)
		if strings.TrimSpace(t.Window) != "" {
			meta += " / " + strings.TrimSpace(t.Window)
		}
		if cwd := displayCWD(t.CWD); cwd != "" {
			meta += " · " + cwd
		}
		if branch := strings.TrimSpace(t.Branch); branch != "" {
			meta += "  " + branch
		}
		fmt.Fprintf(&b, "%s [%s] %s  (%s)\n", mark, t.Status, t.Summary, meta)
	}
	return b.String()
}
