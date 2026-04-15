package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/carlmjohnson/versioninfo"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/robinovitch61/viewport/filterableviewport"
	"github.com/robinovitch61/viewport/viewport"
	"github.com/robinovitch61/viewport/viewport/item"
)

// Version can be set at build time via:
//
//	go build -ldflags "-X main.Version=vX.Y.Z"
var Version = ""

func getVersion() string {
	if Version != "" {
		return Version
	}
	return versioninfo.Short()
}

type sessionProvider string

const (
	providerClaudeCode sessionProvider = "claude"
	providerCodex      sessionProvider = "codex"
)

// conversation holds metadata about a single AI agent session
type conversation struct {
	sessionID    string
	provider     sessionProvider
	cwd          string
	summary      string
	startedAt    time.Time
	modifiedAt   time.Time
	filePath     string
	demoMessages []parsedMessage // populated for demo mode (in-memory, no file)
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
		key.WithHelp("r", "resume"),
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
	logoStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00AAFF"))
	keyStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00FF88"))
	descStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
)

// ASCII logo for jeeves
var asciiLogo = []string{
	"   .-..----..----..-. .-..----. .----.",
	".-.| || {_  | {_  | | | || {_  { {__",
	"| {} || {__ | {__ \\ \\_/ /| {__ .-._} }",
	"`----'`----'`----' `---' `----'`----'",
}

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
	demo       bool

	conversations []conversation
	rows          []conversationRow

	listFV    *filterableviewport.Model[conversationRow]
	previewVP *viewport.Model[previewLine]
	fullFV    *filterableviewport.Model[contentLine]

	ready           bool
	loading         bool
	spinner         spinner.Model
	width, height   int
	lastSelectedIdx int
	resumeSessionID string
	resumeProvider  sessionProvider
	resumeCwd       string
}

func initialModel(searchTerm string, demo bool) model {
	s := spinner.New(spinner.WithSpinner(spinner.Dot))
	s.Style = dimStyle
	return model{
		mode:            viewModeList,
		searchTerm:      searchTerm,
		demo:            demo,
		lastSelectedIdx: -1,
		loading:         true,
		spinner:         s,
	}
}

func (m model) Init() tea.Cmd {
	if m.demo {
		return tea.Batch(m.spinner.Tick, demoSearchCmd(m.searchTerm))
	}
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
		_, _ = fmt.Fprintf(os.Stderr, "search error: %v\n", msg.err)
		return m, tea.Quit

	case conversationContentMsg:
		if m.mode == viewModeFullscreen {
			lines := make([]contentLine, len(msg.lines))
			for i, l := range msg.lines {
				lines[i] = contentLine{line: item.NewItem(l)}
			}
			m.fullFV.SetObjects(lines)
			if m.searchTerm != "" {
				m.fullFV.SetFilter(m.searchTerm, filterableviewport.FilterRegex)
			}
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
			m = m.initViewports()
			m.ready = true
			if len(m.rows) > 0 {
				m.listFV.SetObjects(m.rows)
				m.updatePreview()
			}
		} else {
			m = m.resizeViewports()
		}
	}

	if m.ready {
		switch m.mode {
		case viewModeList:
			m.listFV, cmd = m.listFV.Update(msg)
			cmds = append(cmds, cmd)
			m.previewVP, cmd = m.previewVP.Update(msg)
			cmds = append(cmds, cmd)
			m = m.checkSelectionChanged()
		case viewModeFullscreen:
			m.fullFV, cmd = m.fullFV.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m model) updateListMode(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	if m.listFV.IsCapturingInput() {
		m.listFV, cmd = m.listFV.Update(msg)
		cmds = append(cmds, cmd)
		m = m.checkSelectionChanged()
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
			m.resumeProvider = sel.conv.provider
			m.resumeCwd = sel.conv.cwd
			return m, tea.Quit
		}
	}

	m.listFV, cmd = m.listFV.Update(msg)
	cmds = append(cmds, cmd)
	m.checkSelectionChanged()
	return m, tea.Batch(cmds...)
}

func (m model) updateFullscreenMode(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	if m.fullFV.IsCapturingInput() {
		m.fullFV, cmd = m.fullFV.Update(msg)
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)
	}

	switch {
	case key.Matches(msg, appKeyMap.escape):
		if m.fullFV.GetFilterText() != "" {
			m.fullFV.SetFilter("", filterableviewport.FilterRegex)
			return m, nil
		}
		m.mode = viewModeList
		m.listFV.SetWidth(m.listWidth())
		m.listFV.SetHeight(m.listPaneHeight())
		m.previewVP.SetWidth(m.previewWidth())
		m.previewVP.SetHeight(m.height)
		return m, nil

	case key.Matches(msg, appKeyMap.quit):
		return m, tea.Quit

	case msg.String() == "w":
		m.fullFV.SetWrapText(!m.fullFV.GetWrapText())
		return m, nil
	}

	m.fullFV, cmd = m.fullFV.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m model) View() tea.View {
	var content string
	if !m.ready || m.loading {
		if m.searchTerm == "" {
			content = m.spinner.View() + " Loading..."
		} else {
			content = m.spinner.View() + " Searching..."
		}
	} else {
		switch m.mode {
		case viewModeList:
			if len(m.conversations) == 0 {
				if m.searchTerm == "" {
					content = "No conversations found"
				} else {
					content = fmt.Sprintf("No conversations matching %q", m.searchTerm)
				}
			} else {
				sep := m.renderSeparator()
				leftCol := lipgloss.JoinVertical(lipgloss.Left, m.renderHeader(), m.listFV.View())
				content = lipgloss.JoinHorizontal(lipgloss.Top, leftCol, sep, m.previewVP.View())
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

func (m model) renderHeader() string {
	// controls: key • description pairs
	type control struct{ key, desc string }
	controls := []control{
		{"↑↓", "nav"},
		{"enter", "open"},
		{"r", "resume"},
		{"/", "filter"},
		{"q", "quit"},
	}

	var ctrlParts []string
	for _, c := range controls {
		ctrlParts = append(ctrlParts, keyStyle.Render(c.key)+" "+descStyle.Render(c.desc))
	}
	ctrlLine := strings.Join(ctrlParts, descStyle.Render(" · "))

	var styledLogoLines []string
	for _, l := range asciiLogo {
		styledLogoLines = append(styledLogoLines, logoStyle.Render(l))
	}

	return lipgloss.JoinVertical(lipgloss.Center, lipgloss.JoinVertical(lipgloss.Left, styledLogoLines...), ctrlLine)
}

func (m model) listWidth() int {
	return m.width * 2 / 5
}

func (m model) previewWidth() int {
	return m.width - m.listWidth() - 1 // 1 for separator
}

func (m model) renderSeparator() string {
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

func (m model) listPaneHeight() int {
	return m.height - lipgloss.Height(m.renderHeader())
}

func (m model) initViewports() model {
	// list pane (left)
	listVP := viewport.New[conversationRow](
		m.listWidth(),
		m.listPaneHeight(),
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

	// preview pane (right) — non-interactive, highlights set from search term
	m.previewVP = viewport.New[previewLine](
		m.previewWidth(),
		m.height,
		viewport.WithKeyMap[previewLine](viewport.KeyMap{}),
		viewport.WithStyles[previewLine](viewport.DefaultStyles()),
		viewport.WithProgressBarEnabled[previewLine](true),
	)
	m.previewVP.SetWrapText(true)
	m.previewVP.SetSelectionEnabled(false)

	// fullscreen pane
	fullVP := viewport.New[contentLine](
		m.width,
		m.height,
		viewport.WithKeyMap[contentLine](viewportKeyMap),
		viewport.WithStyles[contentLine](viewport.DefaultStyles()),
		viewport.WithProgressBarEnabled[contentLine](true),
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
	return m
}

func (m model) resizeViewports() model {
	switch m.mode {
	case viewModeList:
		m.listFV.SetWidth(m.listWidth())
		m.listFV.SetHeight(m.listPaneHeight())
		m.previewVP.SetWidth(m.previewWidth())
		m.previewVP.SetHeight(m.height)
		// rebuild rows with new width
		if len(m.conversations) > 0 {
			m.rows = buildRows(m.conversations, m.listWidth())
			m.listFV.SetObjects(m.rows)
		}
	case viewModeFullscreen:
		m.fullFV.SetWidth(m.width)
		m.fullFV.SetHeight(m.height)
	}
	return m
}

func (m model) checkSelectionChanged() model {
	idx := m.listFV.GetSelectedItemIdx()
	if idx != m.lastSelectedIdx {
		m.lastSelectedIdx = idx
		m.updatePreview()
	}
	return m
}

func (m model) updatePreview() {
	sel := m.listFV.GetSelectedItem()
	if sel == nil {
		m.previewVP.SetObjects(nil)
		return
	}
	lines := loadPreview(sel.conv)
	objects := make([]previewLine, len(lines))
	for i, l := range lines {
		objects[i] = previewLine{line: item.NewItem(l)}
	}
	m.previewVP.SetObjects(objects)

	if m.searchTerm == "" {
		m.previewVP.SetHighlights(nil)
		return
	}

	re, err := regexp.Compile(m.searchTerm)
	if err != nil {
		m.previewVP.SetHighlights(nil)
		return
	}

	highlightStyle := lipgloss.NewStyle().Reverse(true).Foreground(lipgloss.BrightRed)
	var highlights []viewport.Highlight
	firstMatchItemIdx := -1
	var firstMatchWidthRange item.WidthRange
	for i, obj := range objects {
		matches := obj.line.ExtractRegexMatches(re)
		for _, match := range matches {
			if firstMatchItemIdx == -1 {
				firstMatchItemIdx = i
				firstMatchWidthRange = match.WidthRange
			}
			highlights = append(highlights, viewport.Highlight{
				ItemIndex: i,
				ItemHighlight: item.Highlight{
					Style:                    highlightStyle,
					ByteRangeUnstyledContent: match.ByteRange,
				},
			})
		}
	}
	m.previewVP.SetHighlights(highlights)

	if firstMatchItemIdx >= 0 {
		m.previewVP.EnsureItemInView(firstMatchItemIdx, firstMatchWidthRange.Start, firstMatchWidthRange.End, 1000000, 50)
	}
}

// search and parsing

func searchCmd(searchTerm string) tea.Cmd {
	return func() tea.Msg {
		var re *regexp.Regexp
		if searchTerm != "" {
			var err error
			re, err = regexp.Compile(searchTerm)
			if err != nil {
				return searchErrorMsg{err: fmt.Errorf("invalid regex %q: %w", searchTerm, err)}
			}
		}

		homeDir, err := os.UserHomeDir()
		if err != nil {
			return searchErrorMsg{err: err}
		}

		conversations, err := discoverConversations(homeDir, re)
		if err != nil {
			return searchErrorMsg{err: err}
		}

		// sort by most recently modified first
		sort.Slice(conversations, func(i, j int) bool {
			return conversations[i].modifiedAt.After(conversations[j].modifiedAt)
		})

		return searchResultsMsg{conversations: conversations}
	}
}

func discoverConversations(homeDir string, re *regexp.Regexp) ([]conversation, error) {
	var conversations []conversation

	claudeConversations, err := discoverClaudeConversations(filepath.Join(homeDir, ".claude", "projects"), re)
	if err != nil {
		return nil, err
	}
	conversations = append(conversations, claudeConversations...)

	codexConversations, err := discoverCodexConversations(filepath.Join(homeDir, ".codex", "sessions"), re)
	if err != nil {
		return nil, err
	}
	conversations = append(conversations, codexConversations...)

	return conversations, nil
}

func discoverClaudeConversations(projectsDir string, re *regexp.Regexp) ([]conversation, error) {
	projectEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading claude projects dir: %w", err)
	}

	var conversations []conversation
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
			conv, hasMatch := parseClaudeSessionMetadata(filePath, re)
			if !hasMatch {
				continue
			}
			conversations = append(conversations, conv)
		}
	}

	return conversations, nil
}

func discoverCodexConversations(sessionsDir string, re *regexp.Regexp) ([]conversation, error) {
	if _, err := os.Stat(sessionsDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat codex sessions dir: %w", err)
	}

	var conversations []conversation
	err := filepath.WalkDir(sessionsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}

		conv, hasMatch := parseCodexSessionMetadata(path, re)
		if hasMatch {
			conversations = append(conversations, conv)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking codex sessions dir: %w", err)
	}

	return conversations, nil
}

// jsonlEntry is used for lightweight Claude Code JSONL parsing.
type jsonlEntry struct {
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message,omitempty"`
	Timestamp string          `json:"timestamp"`
	Cwd       string          `json:"cwd"`
	SessionID string          `json:"sessionId"`
}

type codexJSONLEntry struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type codexSessionMetaPayload struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
}

type codexMessagePayload struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
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

func isTextBlockType(blockType string) bool {
	return blockType == "text" || blockType == "input_text" || blockType == "output_text"
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
			if isTextBlockType(b.Type) && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

// extractSummary finds the first line of user-authored text from message content,
// skipping system-injected content blocks that start with <.
func extractSummary(raw json.RawMessage) string {
	// returns the first line if the text doesn't start with a < tag,
	// indicating system-injected content
	check := func(text string) string {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "<") {
			return ""
		}
		return firstLine(trimmed)
	}

	// try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return check(s)
	}

	// try array of content blocks — check each block independently
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if isTextBlockType(b.Type) && b.Text != "" {
				if s := check(b.Text); s != "" {
					return s
				}
			}
		}
	}

	return ""
}

func parseTimestamp(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseClaudeSessionMetadata parses a Claude Code session JSONL file.
func parseClaudeSessionMetadata(filePath string, re *regexp.Regexp) (conversation, bool) {
	f, err := os.Open(filePath) //nolint:gosec // intentional user-provided file path
	if err != nil {
		return conversation{}, false
	}
	defer func() { _ = f.Close() }()

	sessionID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")

	var conv conversation
	conv.sessionID = sessionID
	conv.provider = providerClaudeCode
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
		if t := parseTimestamp(entry.Timestamp); !t.IsZero() {
			if firstTimestamp.IsZero() {
				firstTimestamp = t
			}
			lastTimestamp = t
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
				if !hasMatch && (re == nil || re.MatchString(text)) {
					hasMatch = true
				}

				// extract summary from first user message text block not starting with <
				if !foundSummary && msg.Role == "user" {
					if s := extractSummary(msg.Content); s != "" {
						conv.summary = truncate(s, 200)
						foundSummary = true
					}
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

// parseCodexSessionMetadata parses a Codex session JSONL file.
func parseCodexSessionMetadata(filePath string, re *regexp.Regexp) (conversation, bool) {
	f, err := os.Open(filePath) //nolint:gosec // intentional user-provided file path
	if err != nil {
		return conversation{}, false
	}
	defer func() { _ = f.Close() }()

	var conv conversation
	conv.sessionID = strings.TrimSuffix(filepath.Base(filePath), ".jsonl")
	conv.provider = providerCodex
	conv.filePath = filePath

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // 10MB max line

	var firstTimestamp, lastTimestamp time.Time
	foundSummary := false
	hasMatch := false

	for scanner.Scan() {
		var entry codexJSONLEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}

		if t := parseTimestamp(entry.Timestamp); !t.IsZero() {
			if firstTimestamp.IsZero() {
				firstTimestamp = t
			}
			lastTimestamp = t
		}

		switch entry.Type {
		case "session_meta":
			var payload codexSessionMetaPayload
			if err := json.Unmarshal(entry.Payload, &payload); err != nil {
				continue
			}
			if payload.ID != "" {
				conv.sessionID = payload.ID
			}
			if conv.cwd == "" && payload.Cwd != "" {
				conv.cwd = payload.Cwd
			}
			if t := parseTimestamp(payload.Timestamp); !t.IsZero() && firstTimestamp.IsZero() {
				firstTimestamp = t
			}

		case "response_item":
			var payload codexMessagePayload
			if err := json.Unmarshal(entry.Payload, &payload); err != nil {
				continue
			}
			if payload.Type != "message" || (payload.Role != "user" && payload.Role != "assistant") {
				continue
			}

			text := extractText(payload.Content)
			if !hasMatch && (re == nil || re.MatchString(text)) {
				hasMatch = true
			}

			if !foundSummary && payload.Role == "user" {
				if s := extractSummary(payload.Content); s != "" {
					conv.summary = truncate(s, 200)
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
	msgs := conv.demoMessages
	if msgs == nil {
		msgs = parseMessages(conv, 0)
	}
	return formatMessages(msgs)
}

func loadConversationCmd(conv conversation) tea.Cmd {
	return func() tea.Msg {
		msgs := conv.demoMessages
		if msgs == nil {
			msgs = parseMessages(conv, 0)
		}
		lines := formatMessages(msgs)
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

func parseMessages(conv conversation, lastN int) []parsedMessage {
	switch conv.provider {
	case providerCodex:
		return parseCodexMessages(conv.filePath, lastN)
	default:
		return parseClaudeMessages(conv.filePath, lastN)
	}
}

func parseClaudeMessages(filePath string, lastN int) []parsedMessage {
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

func parseCodexMessages(filePath string, lastN int) []parsedMessage {
	f, err := os.Open(filePath) //nolint:gosec // intentional user-provided file path
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	var messages []parsedMessage

	for scanner.Scan() {
		var entry codexJSONLEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != "response_item" {
			continue
		}

		var payload codexMessagePayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			continue
		}
		if payload.Type != "message" || (payload.Role != "user" && payload.Role != "assistant") {
			continue
		}

		text := extractText(payload.Content)
		if text == "" {
			continue
		}

		messages = append(messages, parsedMessage{
			role: payload.Role,
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
	// format: provider | summary | cwd | modified
	modified := relativeTime(conv.modifiedAt)
	cwd := shortenPath(conv.cwd)
	provider := providerLabel(conv.provider)

	modWidth := len(modified) + 1
	cwdWidth := min(len(cwd), 30) + 1
	providerWidth := len(provider) + 1
	summaryWidth := width - providerWidth - modWidth - cwdWidth - 4 // padding
	if summaryWidth < 10 {
		summaryWidth = 10
	}

	summary := truncate(conv.summary, summaryWidth)

	return fmt.Sprintf("%s %-*s %s %s",
		dimStyle.Render(provider),
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

func providerLabel(provider sessionProvider) string {
	switch provider {
	case providerClaudeCode:
		return "[claude]"
	case providerCodex:
		return "[codex]"
	default:
		return "[demo]"
	}
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
	var demo bool
	var args []string
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--demo":
			demo = true
		case "-V", "--version":
			fmt.Printf("jeeves %s\n", getVersion())
			os.Exit(0)
		default:
			args = append(args, arg)
		}
	}
	searchTerm := strings.Join(args, " ")

	p := tea.NewProgram(initialModel(searchTerm, demo))
	result, err := p.Run()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// check if we need to resume a conversation
	var resumeSessionID, resumeCwd string
	var resumeProvider sessionProvider
	switch fm := result.(type) {
	case model:
		resumeSessionID = fm.resumeSessionID
		resumeProvider = fm.resumeProvider
		resumeCwd = fm.resumeCwd
	case *model:
		resumeSessionID = fm.resumeSessionID
		resumeProvider = fm.resumeProvider
		resumeCwd = fm.resumeCwd
	}
	if resumeSessionID != "" {
		if resumeCwd != "" {
			if err := os.Chdir(resumeCwd); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "chdir to %s: %v\n", resumeCwd, err) //nolint:gosec // writing to stderr, not a web response
				os.Exit(1)
			}
		}
		binaryName := "claude"
		command := []string{"claude", "--resume", resumeSessionID}
		switch resumeProvider {
		case providerCodex:
			binaryName = "codex"
			command = []string{"codex", "resume", resumeSessionID}
		}

		binary, err := exec.LookPath(binaryName)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "%s not found: %v\n", binaryName, err)
			os.Exit(1)
		}
		if err := syscall.Exec(binary, command, os.Environ()); err != nil { //nolint:gosec // intentional: exec into the selected agent CLI
			_, _ = fmt.Fprintf(os.Stderr, "exec error: %v\n", err)
			os.Exit(1)
		}
	}
}
