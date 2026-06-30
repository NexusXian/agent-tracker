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
	statusInProgress        = "in_progress"
	statusNeedsConfirmation = "needs_confirmation"
	statusCompleted         = "completed"
)

var (
	styleTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	styleMeta    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleMuted   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleLive    = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	styleConfirm = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	styleReview  = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	styleDone    = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	styleError   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleSelBG   = lipgloss.Color("236")

	// Distinct colors per kind of metadata.
	styleCWD     = lipgloss.NewStyle().Foreground(lipgloss.Color("111")) // directory — cornflower blue
	styleBranch  = lipgloss.NewStyle().Foreground(lipgloss.Color("114")) // git branch — green
	styleNote    = lipgloss.NewStyle().Foreground(lipgloss.Color("180")) // notes — tan/gold
	styleNoteSel = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("130"))
)

type keyConfig struct {
	Up, Down, Open, Cancel, Toggle, Delete, Help, Refresh, AddNote string
}

func defaultKeys() keyConfig {
	return keyConfig{Up: "k", Down: "j", Open: "enter", Cancel: "esc",
		Toggle: "c", Delete: "D", Help: "?", Refresh: "r", AddNote: "a"}
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
			case "add_note":
				k.AddNote = v
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
type stateMsg struct {
	env *ipc.Envelope
	err error
}
type cmdResultMsg struct{ err error }

type model struct {
	keys       keyConfig
	width      int
	height     int
	state      ipc.Envelope
	cursor     int
	err        string
	help       bool
	noteMode   bool
	noteInput  string
	noteSelect bool
	noteCursor int
	ctx        tmuxCtx
	loaded     bool
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
		if m.noteMode {
			return m.handleNoteKey(msg)
		}
		return m.handleKey(msg.String())
	}
	return m, nil
}

func (m *model) handleKey(key string) (tea.Model, tea.Cmd) {
	if m.noteSelect {
		return m.handleNoteSelectKey(key)
	}
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
	case m.keys.AddNote:
		if m.selected() != nil {
			m.noteMode = true
			m.noteInput = ""
		}
		return m, nil
	case m.keys.Refresh, "r":
		return m, poll()
	case "x":
		if t := m.selected(); t != nil && len(t.Notes) > 0 {
			m.noteSelect = true
			m.noteCursor = len(t.Notes) - 1
		}
		return m, nil
	}
	return m, nil
}

// handleNoteSelectKey drives the "pick a note to delete" overlay.
func (m *model) handleNoteSelectKey(key string) (tea.Model, tea.Cmd) {
	t := m.selected()
	if t == nil || len(t.Notes) == 0 {
		m.noteSelect = false
		return m, nil
	}
	switch key {
	case m.keys.Cancel, "esc", "q", "ctrl+c":
		m.noteSelect = false
		return m, nil
	case m.keys.Up, "up":
		if m.noteCursor > 0 {
			m.noteCursor--
		}
		return m, nil
	case m.keys.Down, "down":
		if m.noteCursor < len(t.Notes)-1 {
			m.noteCursor++
		}
		return m, nil
	case m.keys.Open, "enter", "x", m.keys.Delete, "d":
		idx := m.noteCursor
		m.noteSelect = false
		return m, m.deleteNoteAt(idx)
	}
	return m, nil
}

func (m *model) handleNoteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.noteMode = false
		m.noteInput = ""
		return m, nil
	case "enter":
		note := strings.TrimSpace(m.noteInput)
		m.noteMode = false
		m.noteInput = ""
		if note == "" {
			return m, nil
		}
		return m, m.addNoteSelected(note)
	case "backspace", "ctrl+h":
		if len(m.noteInput) > 0 {
			r := []rune(m.noteInput)
			m.noteInput = string(r[:len(r)-1])
		}
		return m, nil
	}
	if len(msg.Runes) > 0 {
		m.noteInput += string(msg.Runes)
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

func isInPopup() bool {
	// Most reliable: the tmux key-binding launches us with this env var set
	// inside display-popup. (#{popup_id}/$TMUX_PANE are unreliable from a
	// popup because display-message reports the underlying pane.)
	if strings.TrimSpace(os.Getenv("AGENT_TRACKER_POPUP")) != "" {
		return true
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#{popup_id}").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

func (m *model) focusSelected() tea.Cmd {
	t := m.selected()
	if t == nil {
		return nil
	}
	return func() tea.Msg {
		err := focusTask(*t)
		if err != nil {
			return cmdResultMsg{err: err}
		}
		if isInPopup() {
			return tea.QuitMsg{}
		}
		return cmdResultMsg{}
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

func (m *model) addNoteSelected(note string) tea.Cmd {
	t := m.selected()
	if t == nil {
		return nil
	}
	task := *t
	return func() tea.Msg {
		env := ipc.Envelope{SessionID: task.SessionID, WindowID: task.WindowID, Pane: task.Pane, Note: note, CWD: task.CWD, Branch: task.Branch}
		return cmdResultMsg{err: sendCommand("add_note", &env)}
	}
}

func (m *model) deleteNoteAt(index int) tea.Cmd {
	t := m.selected()
	if t == nil || index < 0 || index >= len(t.Notes) {
		return nil
	}
	task := *t
	idx := index
	return func() tea.Msg {
		env := ipc.Envelope{SessionID: task.SessionID, WindowID: task.WindowID, Pane: task.Pane, NoteIndex: &idx}
		return cmdResultMsg{err: sendCommand("delete_note", &env)}
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
	if m.noteMode {
		b.WriteString("\n")
		b.WriteString(styleConfirm.Render("note: " + m.noteInput + "▏"))
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
	switch t.Status {
	case statusNeedsConfirmation:
		indicatorStyle = styleConfirm
		titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
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
	}
	title := fit(strings.TrimSpace(t.Summary), width-4)

	// Build the meta line as colored segments so directory/branch stand out.
	sep := func(s string) metaSegment { return metaSegment{text: s, style: styleMeta} }
	segs := []metaSegment{{text: strings.TrimSpace(t.Session), style: styleMeta}}
	if w := strings.TrimSpace(t.Window); w != "" {
		segs = append(segs, sep("  /  "), metaSegment{text: w, style: styleMeta})
	}
	if cwd := displayCWD(t.CWD); cwd != "" {
		segs = append(segs, sep("  ·  "), metaSegment{text: cwd, style: styleCWD})
	}
	if branch := strings.TrimSpace(t.Branch); branch != "" {
		segs = append(segs, sep("   "), metaSegment{text: branch, style: styleBranch})
	}
	if t.Status == statusCompleted && !t.Acknowledged {
		segs = append(segs, sep("  ·  awaiting review"))
	}
	if t.Status == statusNeedsConfirmation {
		segs = append(segs, sep("  ·  needs confirmation"))
	}
	if d := liveDuration(t, now); d != "" {
		segs = append(segs, sep("  ·  "), metaSegment{text: d, style: styleMeta})
	}
	metaLine := renderMetaLine(segs, width-2, selected, styleSelBG)
	row := " " + indicatorStyle.Render(indicator) + " " + titleStyle.Render(title) + "\n   " + metaLine

	if selected && len(t.Notes) > 0 {
		limit := 4
		start := len(t.Notes) - limit
		if start < 0 {
			start = 0
		}
		if m.noteSelect {
			// Keep the cursor in view within the visible window.
			start = m.noteCursor - limit + 1
			if start < 0 {
				start = 0
			}
		}
		end := start + limit
		if end > len(t.Notes) {
			end = len(t.Notes)
		}
		for i := start; i < end; i++ {
			text := fit("- "+strings.TrimSpace(t.Notes[i].Text), width-4)
			style := styleNote
			if m.noteSelect && i == m.noteCursor {
				style = styleNoteSel
			}
			row += "\n   " + style.Render(text)
		}
		if m.noteSelect {
			hint := fmt.Sprintf("delete note %d/%d  ·  %s/%s move  ·  enter delete  ·  esc cancel",
				m.noteCursor+1, len(t.Notes), m.keys.Up, m.keys.Down)
			row += "\n   " + styleConfirm.Render(fit(hint, width-2))
		}
	}
	return row
}

type metaSegment struct {
	text  string
	style lipgloss.Style
}

// renderMetaLine concatenates colored segments, truncates to width, and pads the
// remainder so the selection background (if any) fills the whole line.
func renderMetaLine(segs []metaSegment, width int, selected bool, bg lipgloss.Color) string {
	if width < 0 {
		width = 0
	}
	var b strings.Builder
	used := 0
	for _, s := range segs {
		if used >= width {
			break
		}
		text := s.text
		w := lipgloss.Width(text)
		if used+w > width {
			text = truncateToWidth(text, width-used)
			w = lipgloss.Width(text)
		}
		style := s.style
		if selected {
			style = style.Background(bg)
		}
		b.WriteString(style.Render(text))
		used += w
	}
	if used < width {
		pad := strings.Repeat(" ", width-used)
		if selected {
			b.WriteString(lipgloss.NewStyle().Background(bg).Render(pad))
		} else {
			b.WriteString(pad)
		}
	}
	return b.String()
}

func truncateToWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func (m model) renderHelp(width int) string {
	lines := []string{
		fmt.Sprintf("%s / %s   move up / down", m.keys.Up, m.keys.Down),
		fmt.Sprintf("%s          open the task's tmux pane", m.keys.Open),
		fmt.Sprintf("%s          finish an in-progress task / acknowledge a completed one", m.keys.Toggle),
		fmt.Sprintf("%s          delete the selected task", m.keys.Delete),
		fmt.Sprintf("%s          add a note to the selected task", m.keys.AddNote),
		"x          pick a note of the selected task to delete",
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
	if m.noteSelect {
		return fmt.Sprintf("%s/%s pick note  enter delete  esc cancel", m.keys.Up, m.keys.Down)
	}
	return fmt.Sprintf("%s/%s move  %s open  %s toggle  %s note  x delnote  %s delete  %s refresh  %s quit",
		m.keys.Up, m.keys.Down, m.keys.Open, m.keys.Toggle, m.keys.AddNote, m.keys.Delete, m.keys.Refresh, m.keys.Cancel)
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
	case statusNeedsConfirmation:
		return "!"
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
	case statusNeedsConfirmation:
		return 0
	case statusInProgress:
		return 1
	case statusCompleted:
		return 2
	}
	return 3
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
		req.Note = env.Note
		req.NoteIndex = env.NoteIndex
		req.CWD = env.CWD
		req.Branch = env.Branch
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
		case statusNeedsConfirmation:
			mark = "!"
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
			meta += "  " + branch
		}
		fmt.Fprintf(&b, "%s [%s] %s  (%s)\n", mark, t.Status, t.Summary, meta)
		for _, note := range t.Notes {
			fmt.Fprintf(&b, "  - %s\n", strings.TrimSpace(note.Text))
		}
	}
	return b.String()
}
