package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/robinovitch61/viewport/filterableviewport"
	"github.com/robinovitch61/viewport/viewport"
	"github.com/robinovitch61/viewport/viewport/item"
)

// conversation holds metadata about a single Claude session
type conversation struct {
	sessionID  string
	cwd        string
	summary    string
	startedAt  time.Time
	modifiedAt time.Time
	filePath   string
}

// conversationRow is a viewport Object for the left pane list
type conversationRow struct {
	conv conversation
	line item.SingleItem
}

func (r conversationRow) GetItem() item.Item {
	return r.line
}

// previewLine is a viewport Object for the right preview pane
type previewLine struct {
	line item.SingleItem
}

func (p previewLine) GetItem() item.Item {
	return p.line
}

// contentLine is a viewport Object for fullscreen conversation view
type contentLine struct {
	line item.SingleItem
}

func (c contentLine) GetItem() item.Item {
	return c.line
}

// viewMode represents the current UI mode
type viewMode int

const (
	viewModeList       viewMode = iota // split pane: list + preview
	viewModeFullscreen                 // fullscreen conversation view
)

// appKeys defines the application-level key bindings
type appKeys struct {
	quit   key.Binding
	enter  key.Binding
	escape key.Binding
	resume key.Binding
}

var appKeyMap = appKeys{
	quit: key.NewBinding(
		key.WithKeys("ctrl+c", "q"),
		key.WithHelp("ctrl+c/q", "quit"),
	),
	enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "open"),
	),
	escape: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "back"),
	),
	resume: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "resume in claude"),
	),
}

var viewportKeyMap = viewport.DefaultKeyMap()
var filterableViewportKeyMap = filterableviewport.DefaultKeyMap()

// styles
var (
	userStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00AAFF"))
	assistantStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00FF88"))
	dimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#666666"))
	separatorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#444444"))
)

// message types
type searchResultsMsg struct {
	conversations []conversation
}

type conversationContentMsg struct {
	sessionID string
	lines     []string
}

type searchErrorMsg struct {
	err error
}

// model holds all application state
type model struct {
	mode       viewMode
	searchTerm string

	conversations []conversation
	rows          []conversationRow

	listFV    *filterableviewport.Model[conversationRow]
	previewFV *filterableviewport.Model[previewLine]
	fullFV    *filterableviewport.Model[contentLine]

	ready           bool
	loading         bool
	spinner         spinner.Model
	width, height   int
	lastSelectedIdx int
	resumeSessionID string
	resumeCwd       string
}

func initialModel(searchTerm string) model {
	s := spinner.New(spinner.WithSpinner(spinner.Dot))
	s.Style = dimStyle
	return model{
		mode:            viewModeList,
		searchTerm:      searchTerm,
		lastSelectedIdx: -1,
		loading:         true,
		spinner:         s,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, searchCmd(m.searchTerm))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case searchResultsMsg:
		m.loading = false
		m.conversations = msg.conversations
		m.rows = buildRows(m.conversations, m.listWidth())
		if m.ready {
			m.listFV.SetObjects(m.rows)
			m.updatePreview()
		}
		return m, nil

	case searchErrorMsg:
		// show error and quit
		fmt.Fprintf(os.Stderr, "search error: %v\n", msg.err)
		return m, tea.Quit

	case conversationContentMsg:
		if m.mode == viewModeFullscreen {
			lines := make([]contentLine, len(msg.lines))
			for i, l := range msg.lines {
				lines[i] = contentLine{line: item.NewItem(l)}
			}
			m.fullFV.SetObjects(lines)
		}
		return m, nil

	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

		if !m.ready {
			return m, nil
		}

		switch m.mode {
		case viewModeList:
			return m.updateListMode(msg)
		case viewModeFullscreen:
			return m.updateFullscreenMode(msg)
		}

	case spinner.TickMsg:
		if m.loading {
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if !m.ready {
			m.initViewports()
			m.ready = true
			if len(m.rows) > 0 {
				m.listFV.SetObjects(m.rows)
				m.updatePreview()
			}
		} else {
			m.resizeViewports()
		}
	}

	if m.ready {
		switch m.mode {
		case viewModeList:
			m.listFV, cmd = m.listFV.Update(msg)
			cmds = append(cmds, cmd)
			m.previewFV, cmd = m.previewFV.Update(msg)
			cmds = append(cmds, cmd)
			m.checkSelectionChanged()
		case viewModeFullscreen:
			m.fullFV, cmd = m.fullFV.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m *model) updateListMode(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	if m.listFV.IsCapturingInput() {
		m.listFV, cmd = m.listFV.Update(msg)
		cmds = append(cmds, cmd)
		m.checkSelectionChanged()
		return m, tea.Batch(cmds...)
	}

	switch {
	case key.Matches(msg, appKeyMap.quit):
		return m, tea.Quit

	case key.Matches(msg, appKeyMap.enter):
		if sel := m.listFV.GetSelectedItem(); sel != nil {
			m.mode = viewModeFullscreen
			m.fullFV.SetWidth(m.width)
			m.fullFV.SetHeight(m.height)
			return m, loadConversationCmd(sel.conv)
		}

	case key.Matches(msg, appKeyMap.resume):
		if sel := m.listFV.GetSelectedItem(); sel != nil {
			m.resumeSessionID = sel.conv.sessionID
			m.resumeCwd = sel.conv.cwd
			return m, tea.Quit
		}
	}

	m.listFV, cmd = m.listFV.Update(msg)
	cmds = append(cmds, cmd)
	m.checkSelectionChanged()
	return m, tea.Batch(cmds...)
}

func (m *model) updateFullscreenMode(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	if m.fullFV.IsCapturingInput() {
		m.fullFV, cmd = m.fullFV.Update(msg)
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)
	}

	switch {
	case key.Matches(msg, appKeyMap.escape):
		m.mode = viewModeList
		m.listFV.SetWidth(m.listWidth())
		m.listFV.SetHeight(m.height)
		m.previewFV.SetWidth(m.previewWidth())
		m.previewFV.SetHeight(m.height)
		return m, nil

	case key.Matches(msg, appKeyMap.quit):
		return m, tea.Quit
	}

	m.fullFV, cmd = m.fullFV.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m model) View() tea.View {
	var content string
	if !m.ready || m.loading {
		content = m.spinner.View() + " Searching..."
	} else {
		switch m.mode {
		case viewModeList:
			if len(m.conversations) == 0 {
				content = fmt.Sprintf("No conversations matching %q", m.searchTerm)
			} else {
				sep := m.renderSeparator()
				content = lipgloss.JoinHorizontal(lipgloss.Top, m.listFV.View(), sep, m.previewFV.View())
			}
		case viewModeFullscreen:
			content = m.fullFV.View()
		}
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// layout helpers

func (m *model) listWidth() int {
	return m.width * 2 / 5
}

func (m *model) previewWidth() int {
	return m.width - m.listWidth() - 1 // 1 for separator
}

func (m *model) renderSeparator() string {
	var sb strings.Builder
	for range m.height {
		sb.WriteString(separatorStyle.Render("│"))
		sb.WriteByte('\n')
	}
	s := sb.String()
	if len(s) > 0 {
		s = s[:len(s)-1] // trim trailing newline
	}
	return s
}

func (m *model) initViewports() {
	// list pane (left)
	listVP := viewport.New[conversationRow](
		m.listWidth(),
		m.height,
		viewport.WithKeyMap[conversationRow](viewportKeyMap),
		viewport.WithStyles[conversationRow](viewport.DefaultStyles()),
		viewport.WithSelectionEnabled[conversationRow](true),
	)
	m.listFV = filterableviewport.New[conversationRow](
		listVP,
		filterableviewport.WithKeyMap[conversationRow](filterableViewportKeyMap),
		filterableviewport.WithStyles[conversationRow](filterableviewport.DefaultStyles()),
		filterableviewport.WithPrefixText[conversationRow]("Filter:"),
		filterableviewport.WithEmptyText[conversationRow](""),
		filterableviewport.WithMatchingItemsOnly[conversationRow](false),
		filterableviewport.WithCanToggleMatchingItemsOnly[conversationRow](true),
		filterableviewport.WithHorizontalPad[conversationRow](50),
		filterableviewport.WithVerticalPad[conversationRow](5),
	)

	// preview pane (right) — non-interactive, filter hard-set to search term
	previewVP := viewport.New[previewLine](
		m.previewWidth(),
		m.height,
		viewport.WithKeyMap[previewLine](viewport.KeyMap{}),
		viewport.WithStyles[previewLine](viewport.DefaultStyles()),
	)
	m.previewFV = filterableviewport.New[previewLine](
		previewVP,
		filterableviewport.WithKeyMap[previewLine](filterableviewport.KeyMap{}),
		filterableviewport.WithStyles[previewLine](filterableviewport.DefaultStyles()),
		filterableviewport.WithEmptyText[previewLine](""),
		filterableviewport.WithHorizontalPad[previewLine](50),
		filterableviewport.WithVerticalPad[previewLine](10),
	)
	m.previewFV.SetWrapText(true)
	m.previewFV.SetSelectionEnabled(false)

	// fullscreen pane
	fullVP := viewport.New[contentLine](
		m.width,
		m.height,
		viewport.WithKeyMap[contentLine](viewportKeyMap),
		viewport.WithStyles[contentLine](viewport.DefaultStyles()),
	)
	m.fullFV = filterableviewport.New[contentLine](
		fullVP,
		filterableviewport.WithKeyMap[contentLine](filterableViewportKeyMap),
		filterableviewport.WithStyles[contentLine](filterableviewport.DefaultStyles()),
		filterableviewport.WithPrefixText[contentLine]("Filter:"),
		filterableviewport.WithEmptyText[contentLine](""),
		filterableviewport.WithMatchingItemsOnly[contentLine](false),
		filterableviewport.WithCanToggleMatchingItemsOnly[contentLine](true),
		filterableviewport.WithHorizontalPad[contentLine](50),
		filterableviewport.WithVerticalPad[contentLine](10),
	)
	m.fullFV.SetWrapText(true)
	m.fullFV.SetSelectionEnabled(false)
}

func (m *model) resizeViewports() {
	switch m.mode {
	case viewModeList:
		m.listFV.SetWidth(m.listWidth())
		m.listFV.SetHeight(m.height)
		m.previewFV.SetWidth(m.previewWidth())
		m.previewFV.SetHeight(m.height)
		// rebuild rows with new width
		if len(m.conversations) > 0 {
			m.rows = buildRows(m.conversations, m.listWidth())
			m.listFV.SetObjects(m.rows)
		}
	case viewModeFullscreen:
		m.fullFV.SetWidth(m.width)
		m.fullFV.SetHeight(m.height)
	}
}

func (m *model) checkSelectionChanged() {
	idx := m.listFV.GetSelectedItemIdx()
	if idx != m.lastSelectedIdx {
		m.lastSelectedIdx = idx
		m.updatePreview()
	}
}

func (m *model) updatePreview() {
	sel := m.listFV.GetSelectedItem()
	if sel == nil {
		m.previewFV.SetObjects(nil)
		return
	}
	lines := loadPreview(sel.conv)
	objects := make([]previewLine, len(lines))
	for i, l := range lines {
		objects[i] = previewLine{line: item.NewItem(l)}
	}
	m.previewFV.SetObjects(objects)
	m.previewFV.SetFilter(m.searchTerm, false)
}

// search and parsing

func searchCmd(searchTerm string) tea.Cmd {
	return func() tea.Msg {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return searchErrorMsg{err: err}
		}
		projectsDir := filepath.Join(homeDir, ".claude", "projects")

		// walk all project directories for session JSONL files
		var conversations []conversation
		projectEntries, err := os.ReadDir(projectsDir)
		if err != nil {
			return searchErrorMsg{err: fmt.Errorf("reading projects dir: %w", err)}
		}

		for _, projEntry := range projectEntries {
			if !projEntry.IsDir() {
				continue
			}
			projDir := filepath.Join(projectsDir, projEntry.Name())
			files, err := os.ReadDir(projDir)
			if err != nil {
				continue
			}
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
					continue
				}
				filePath := filepath.Join(projDir, f.Name())
				conv, hasMatch := parseSessionMetadata(filePath, searchTerm)
				if !hasMatch {
					continue
				}
				conversations = append(conversations, conv)
			}
		}

		// sort by most recently modified first
		sort.Slice(conversations, func(i, j int) bool {
			return conversations[i].modifiedAt.After(conversations[j].modifiedAt)
		})

		return searchResultsMsg{conversations: conversations}
	}
}

// jsonlEntry is used for lightweight JSONL parsing
type jsonlEntry struct {
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message,omitempty"`
	Timestamp string          `json:"timestamp"`
	Cwd       string          `json:"cwd"`
	SessionID string          `json:"sessionId"`
}

type msgContent struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name,omitempty"` // for tool_use blocks
}

func extractText(raw json.RawMessage) string {
	// try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// try array of content blocks
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

func parseSessionMetadata(filePath string, searchTerm string) (conversation, bool) {
	f, err := os.Open(filePath) //nolint:gosec // intentional user-provided file path
	if err != nil {
		return conversation{}, false
	}
	defer func() { _ = f.Close() }()

	sessionID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")

	var conv conversation
	conv.sessionID = sessionID
	conv.filePath = filePath

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // 10MB max line

	var firstTimestamp, lastTimestamp time.Time
	foundSummary := false
	hasMatch := false

	for scanner.Scan() {
		var entry jsonlEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}

		// parse timestamp
		if entry.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339Nano, entry.Timestamp); err == nil {
				if firstTimestamp.IsZero() {
					firstTimestamp = t
				}
				lastTimestamp = t
			}
		}

		// extract cwd
		if conv.cwd == "" && entry.Cwd != "" {
			conv.cwd = entry.Cwd
		}

		// only look at user/assistant messages
		if (entry.Type == "user" || entry.Type == "assistant") && len(entry.Message) > 0 {
			var msg msgContent
			if err := json.Unmarshal(entry.Message, &msg); err == nil {
				text := extractText(msg.Content)

				// check for search term match in message text
				if !hasMatch && strings.Contains(text, searchTerm) {
					hasMatch = true
				}

				// extract summary from first user message
				if !foundSummary && msg.Role == "user" {
					conv.summary = truncate(firstLine(text), 200)
					foundSummary = true
				}
			}
		}
	}

	conv.startedAt = firstTimestamp
	conv.modifiedAt = lastTimestamp

	if conv.summary == "" {
		conv.summary = "(no summary)"
	}

	return conv, hasMatch
}

func loadPreview(conv conversation) []string {
	messages := parseMessages(conv.filePath, 0)
	return formatMessages(messages)
}

func loadConversationCmd(conv conversation) tea.Cmd {
	return func() tea.Msg {
		messages := parseMessages(conv.filePath, 0) // 0 = all messages
		lines := formatMessages(messages)
		return conversationContentMsg{
			sessionID: conv.sessionID,
			lines:     lines,
		}
	}
}

type parsedMessage struct {
	role string
	text string
}

func parseMessages(filePath string, lastN int) []parsedMessage {
	f, err := os.Open(filePath) //nolint:gosec // intentional user-provided file path
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	var messages []parsedMessage

	for scanner.Scan() {
		var entry jsonlEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}

		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}
		if len(entry.Message) == 0 {
			continue
		}

		var msg msgContent
		if err := json.Unmarshal(entry.Message, &msg); err != nil {
			continue
		}

		text := extractText(msg.Content)
		if text == "" {
			continue
		}

		messages = append(messages, parsedMessage{
			role: msg.Role,
			text: text,
		})
	}

	if lastN > 0 && len(messages) > lastN {
		messages = messages[len(messages)-lastN:]
	}

	return messages
}

func formatMessages(messages []parsedMessage) []string {
	var lines []string
	for _, msg := range messages {
		var prefix string
		switch msg.role {
		case "user":
			prefix = userStyle.Render("user:")
		case "assistant":
			prefix = assistantStyle.Render("assistant:")
		default:
			prefix = dimStyle.Render(msg.role + ":")
		}

		msgLines := strings.Split(msg.text, "\n")
		lines = append(lines, prefix+" "+msgLines[0])
		for _, l := range msgLines[1:] {
			lines = append(lines, "  "+l)
		}
		lines = append(lines, "") // blank separator
	}
	return lines
}

// row building

func buildRows(convs []conversation, width int) []conversationRow {
	rows := make([]conversationRow, len(convs))
	for i, conv := range convs {
		rows[i] = conversationRow{
			conv: conv,
			line: item.NewItem(formatRow(conv, width)),
		}
	}
	return rows
}

func formatRow(conv conversation, width int) string {
	// format: summary | cwd | modified
	modified := relativeTime(conv.modifiedAt)
	cwd := shortenPath(conv.cwd)

	modWidth := len(modified) + 1
	cwdWidth := min(len(cwd), 30) + 1
	summaryWidth := width - modWidth - cwdWidth - 4 // padding
	if summaryWidth < 10 {
		summaryWidth = 10
	}

	summary := truncate(conv.summary, summaryWidth)

	return fmt.Sprintf("%-*s %s %s",
		summaryWidth, summary,
		dimStyle.Render(fmt.Sprintf("%-*s", cwdWidth, truncate(cwd, cwdWidth))),
		dimStyle.Render(modified),
	)
}

// utility functions

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

func shortenPath(path string) string {
	homeDir, _ := os.UserHomeDir()
	if homeDir != "" && strings.HasPrefix(path, homeDir) {
		return "~" + path[len(homeDir):]
	}
	return path
}

func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", h)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	default:
		return t.Format("Jan 02")
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: jeeves <searchterm>\n")
		os.Exit(1)
	}

	searchTerm := strings.Join(os.Args[1:], " ")

	p := tea.NewProgram(initialModel(searchTerm))
	result, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// check if we need to resume a conversation
	var resumeSessionID, resumeCwd string
	switch fm := result.(type) {
	case model:
		resumeSessionID = fm.resumeSessionID
		resumeCwd = fm.resumeCwd
	case *model:
		resumeSessionID = fm.resumeSessionID
		resumeCwd = fm.resumeCwd
	}
	if resumeSessionID != "" {
		if resumeCwd != "" {
			if err := os.Chdir(resumeCwd); err != nil {
				fmt.Fprintf(os.Stderr, "chdir to %s: %v\n", resumeCwd, err)
				os.Exit(1)
			}
		}
		binary, err := exec.LookPath("claude")
		if err != nil {
			fmt.Fprintf(os.Stderr, "claude not found: %v\n", err)
			os.Exit(1)
		}
		if err := syscall.Exec(binary, []string{"claude", "--resume", resumeSessionID}, os.Environ()); err != nil { //nolint:gosec // intentional: exec into claude with user-selected session
			fmt.Fprintf(os.Stderr, "exec error: %v\n", err)
			os.Exit(1)
		}
	}
}
