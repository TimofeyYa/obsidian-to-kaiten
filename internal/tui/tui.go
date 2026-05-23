// Package tui — экраны Bubble Tea для интерактивного режима.
// Два этапа: ввод URL+токена → плоский список папок (document_group) с поиском.
package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/timofeyblog/kaiten-obsidian-sync/internal/kaiten"
)

// Result — что TUI возвращает после успешного прохождения визарда.
type Result struct {
	BaseURL string
	Token   string
	RootUID string // UID выбранной папки (document_group)
	Aborted bool
}

// ---------- Стили ----------

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4D4D"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#22C55E"))
	hintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#888"))
)

// ---------- Этапы ----------

type stage int

const (
	stageLogin stage = iota
	stageFolderList
	stageDone
)

type loginModel struct {
	inputs   []textinput.Model
	focus    int
	err      string
	checking bool
	spinner  spinner.Model
	user     *kaiten.User
}

// folderModel — экран плоского списка папок.
type folderModel struct {
	client  *kaiten.Client
	list    list.Model
	loading bool
	loadErr string
	spinner spinner.Model
	count   int
}

type model struct {
	stage  stage
	login  loginModel
	folder folderModel
	result Result
}

// New создаёт начальную модель TUI.
func New(initialURL string) tea.Model {
	urlIn := textinput.New()
	urlIn.Placeholder = "https://mycompany.kaiten.ru"
	urlIn.SetValue(initialURL)
	urlIn.Focus()
	urlIn.CharLimit = 200
	urlIn.Width = 60

	tokIn := textinput.New()
	tokIn.Placeholder = "Bearer-токен (из настроек профиля Kaiten)"
	tokIn.EchoMode = textinput.EchoPassword
	tokIn.EchoCharacter = '•'
	tokIn.CharLimit = 200
	tokIn.Width = 60

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	return &model{
		stage: stageLogin,
		login: loginModel{
			inputs:  []textinput.Model{urlIn, tokIn},
			spinner: sp,
		},
	}
}

func (m *model) Init() tea.Cmd { return tea.Batch(textinput.Blink, m.login.spinner.Tick) }

// ---------- Сообщения ----------

type loginOKMsg struct {
	user   *kaiten.User
	client *kaiten.Client
}
type loginErrMsg struct{ err string }

type foldersOKMsg struct {
	entries []list.Item
	count   int
}
type foldersErrMsg struct{ err string }

// checkLogin — async проверка токена.
func checkLogin(baseURL, token string) tea.Cmd {
	return func() tea.Msg {
		c := kaiten.New(baseURL, token)
		ctx := context.Background()
		u, err := c.GetCurrentUser(ctx)
		if err != nil {
			return loginErrMsg{err: err.Error()}
		}
		return loginOKMsg{user: u, client: c}
	}
}

// loadFolders — подгружает плоский список всех document_group инстанса.
func loadFolders(c *kaiten.Client) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		folders, err := c.ListAllDocumentGroups(ctx)
		if err != nil {
			return foldersErrMsg{err: err.Error()}
		}
		items := make([]list.Item, 0, len(folders))
		for _, f := range folders {
			items = append(items, folderItem{
				uid:      f.UID,
				title:    f.Title,
				fullPath: f.FullPath,
			})
		}
		return foldersOKMsg{entries: items, count: len(folders)}
	}
}

// ---------- Update ----------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m.stage {
	case stageLogin:
		return m.updateLogin(msg)
	case stageFolderList:
		return m.updateFolderList(msg)
	}
	return m, tea.Quit
}

func (m *model) updateLogin(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0, 4)
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.result.Aborted = true
			return m, tea.Quit
		case "tab", "shift+tab", "up", "down":
			m.login.focus = (m.login.focus + 1) % len(m.login.inputs)
			for i := range m.login.inputs {
				if i == m.login.focus {
					cmds = append(cmds, m.login.inputs[i].Focus())
				} else {
					m.login.inputs[i].Blur()
				}
			}
		case "enter":
			if m.login.focus < len(m.login.inputs)-1 {
				m.login.focus++
				m.login.inputs[m.login.focus].Focus()
				m.login.inputs[m.login.focus-1].Blur()
			} else {
				url := strings.TrimRight(strings.TrimSpace(m.login.inputs[0].Value()), "/")
				tok := strings.TrimSpace(m.login.inputs[1].Value())
				if url == "" || tok == "" {
					m.login.err = "URL и токен обязательны"
					return m, nil
				}
				m.login.err = ""
				m.login.checking = true
				cmds = append(cmds, checkLogin(url, tok), m.login.spinner.Tick)
			}
		}
	case spinner.TickMsg:
		var c tea.Cmd
		m.login.spinner, c = m.login.spinner.Update(msg)
		if m.login.checking {
			cmds = append(cmds, c)
		}
	case loginOKMsg:
		m.login.checking = false
		m.login.user = msg.user
		m.result.BaseURL = strings.TrimRight(m.login.inputs[0].Value(), "/")
		m.result.Token = m.login.inputs[1].Value()

		// Переход к экрану выбора папки.
		delegate := list.NewDefaultDelegate()
		l := list.New(nil, delegate, 80, 20)
		l.Title = "Выберите папку Kaiten для синхронизации"
		l.SetShowStatusBar(true)
		l.SetFilteringEnabled(true)
		l.SetShowHelp(true)
		m.folder = folderModel{
			client:  msg.client,
			list:    l,
			loading: true,
			spinner: spinner.New(),
		}
		m.folder.spinner.Spinner = spinner.Dot
		m.stage = stageFolderList
		cmds = append(cmds, loadFolders(msg.client), m.folder.spinner.Tick)
		return m, tea.Batch(cmds...)
	case loginErrMsg:
		m.login.checking = false
		m.login.err = msg.err
		return m, nil
	}

	for i := range m.login.inputs {
		var c tea.Cmd
		m.login.inputs[i], c = m.login.inputs[i].Update(msg)
		cmds = append(cmds, c)
	}
	return m, tea.Batch(cmds...)
}

func (m *model) updateFolderList(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0, 4)
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Если активен фильтр — отдаём управление list.Update.
		if m.folder.list.FilterState() == list.Filtering {
			break
		}
		switch msg.String() {
		case "ctrl+c", "esc":
			m.result.Aborted = true
			return m, tea.Quit
		case "enter":
			if it, ok := m.folder.list.SelectedItem().(folderItem); ok {
				m.result.RootUID = it.uid
				m.stage = stageDone
				return m, tea.Quit
			}
		}
	case tea.WindowSizeMsg:
		m.folder.list.SetSize(msg.Width-4, msg.Height-6)
	case foldersOKMsg:
		m.folder.loading = false
		m.folder.count = msg.count
		m.folder.list.SetItems(msg.entries)
	case foldersErrMsg:
		m.folder.loading = false
		m.folder.loadErr = msg.err
	case spinner.TickMsg:
		var c tea.Cmd
		m.folder.spinner, c = m.folder.spinner.Update(msg)
		if m.folder.loading {
			cmds = append(cmds, c)
		}
	}
	var c tea.Cmd
	m.folder.list, c = m.folder.list.Update(msg)
	cmds = append(cmds, c)
	return m, tea.Batch(cmds...)
}

// ---------- View ----------

func (m *model) View() string {
	switch m.stage {
	case stageLogin:
		return m.viewLogin()
	case stageFolderList:
		return m.viewFolderList()
	}
	return ""
}

func (m *model) viewLogin() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("kaiten-obsidian-sync · настройка"))
	b.WriteString("\n\n")
	labels := []string{"Kaiten URL", "Bearer-токен"}
	for i, in := range m.login.inputs {
		b.WriteString(labels[i] + ":\n  " + in.View() + "\n\n")
	}
	if m.login.checking {
		b.WriteString(m.login.spinner.View() + " проверяю соединение…\n")
	}
	if m.login.err != "" {
		b.WriteString(errStyle.Render("ошибка: "+m.login.err) + "\n")
	}
	if m.login.user != nil {
		b.WriteString(okStyle.Render(fmt.Sprintf("вход выполнен как %s <%s>", m.login.user.FullName, m.login.user.Email)) + "\n")
	}
	b.WriteString("\n" + hintStyle.Render("tab — переключение, enter — далее, esc — выход"))
	return b.String()
}

func (m *model) viewFolderList() string {
	var b strings.Builder
	if m.folder.loading {
		b.WriteString(m.folder.spinner.View() + " загружаю список папок Kaiten…\n")
		b.WriteString(hintStyle.Render("это может занять до минуты на крупных инстансах"))
		return b.String()
	}
	if m.folder.loadErr != "" {
		b.WriteString(errStyle.Render("ошибка: " + m.folder.loadErr))
		b.WriteString("\n\n" + hintStyle.Render("esc — выход"))
		return b.String()
	}
	b.WriteString(m.folder.list.View())
	b.WriteString("\n")
	b.WriteString(hintStyle.Render(fmt.Sprintf(
		"всего папок: %d · enter — выбрать · / — поиск · esc — выход",
		m.folder.count,
	)))
	return b.String()
}

// Result возвращает результат после Quit.
func (m *model) Result() Result { return m.result }

// ---------- list.Item ----------

// folderItem — папка с полным путём.
type folderItem struct {
	uid      string
	title    string
	fullPath string
}

func (f folderItem) Title() string       { return "📁 " + f.fullPath }
func (f folderItem) Description() string { return "uid: " + f.uid }
func (f folderItem) FilterValue() string { return f.fullPath }

// Run запускает TUI и возвращает Result. Если пользователь отменил — Result.Aborted = true.
func Run(initialURL string) (Result, error) {
	m := New(initialURL)
	prog := tea.NewProgram(m, tea.WithAltScreen())
	final, err := prog.Run()
	if err != nil {
		return Result{}, err
	}
	mm, _ := final.(*model)
	return mm.Result(), nil
}
