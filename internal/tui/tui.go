// Package tui — экраны Bubble Tea для интерактивного режима.
// Три этапа: ввод URL+токена → навигация по папкам Kaiten → подтверждение.
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
	RootUID string // UID выбранной папки/пространства (root для рекурсивного синка)
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
	stageFolderPick
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

// folderModel — экран навигации по дереву Kaiten.
// Хранит стек уровней (breadcrumbs) и сообщения от backend.
type folderModel struct {
	client  *kaiten.Client
	list    list.Model
	loading bool
	loadErr string
	spinner spinner.Model
	stack   []breadcrumb // путь от корня до текущего уровня
}

type breadcrumb struct {
	UID   string // "" — самый верх (все spaces)
	Title string
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

type childrenOKMsg struct {
	entries []list.Item
}
type childrenErrMsg struct{ err string }

// checkLogin — async проверка токена + создание клиента.
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

// loadChildren — подгружает прямых потомков для текущего уровня.
// Для верхнего уровня (uid == "") — список spaces.
func loadChildren(c *kaiten.Client, parentUID string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		var items []list.Item
		if parentUID == "" {
			// Верхний уровень: показываем пространства через /spaces (быстрее, чем tree-entities root).
			spaces, err := c.ListSpaces(ctx)
			if err != nil {
				return childrenErrMsg{err: "spaces: " + err.Error()}
			}
			for _, s := range spaces {
				items = append(items, treeItem{
					uid:        s.UID,
					title:      s.Title,
					entityType: kaiten.EntityTypeSpace,
				})
			}
			return childrenOKMsg{entries: items}
		}
		children, err := c.ListTreeChildrenAll(ctx, parentUID)
		if err != nil {
			return childrenErrMsg{err: err.Error()}
		}
		for _, ch := range children {
			// Показываем только spaces, folders и documents (без archived).
			if ch.Archived {
				continue
			}
			if !ch.IsSpace() && !ch.IsFolder() && !ch.IsDocument() {
				continue
			}
			items = append(items, treeItem{
				uid:        ch.UID,
				title:      ch.Title,
				entityType: ch.EntityType,
			})
		}
		return childrenOKMsg{entries: items}
	}
}

// ---------- Update ----------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m.stage {
	case stageLogin:
		return m.updateLogin(msg)
	case stageFolderPick:
		return m.updateFolderPick(msg)
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
		if m.login.checking || m.folder.loading {
			cmds = append(cmds, c)
		}
	case loginOKMsg:
		m.login.checking = false
		m.login.user = msg.user
		m.result.BaseURL = strings.TrimRight(m.login.inputs[0].Value(), "/")
		m.result.Token = m.login.inputs[1].Value()

		// Переход к экрану выбора папки. Подгружаем верхний уровень.
		delegate := list.NewDefaultDelegate()
		l := list.New(nil, delegate, 70, 18)
		l.Title = "Выберите папку для синхронизации"
		l.SetShowStatusBar(false)
		m.folder = folderModel{
			client:  msg.client,
			list:    l,
			loading: true,
			spinner: spinner.New(),
			stack:   []breadcrumb{{UID: "", Title: "Все пространства"}},
		}
		m.folder.spinner.Spinner = spinner.Dot
		m.stage = stageFolderPick
		cmds = append(cmds, loadChildren(msg.client, ""), m.folder.spinner.Tick)
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

func (m *model) updateFolderPick(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0, 4)
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.result.Aborted = true
			return m, tea.Quit
		case "backspace", "left":
			// Подняться на уровень выше.
			if len(m.folder.stack) > 1 {
				m.folder.stack = m.folder.stack[:len(m.folder.stack)-1]
				parent := m.folder.stack[len(m.folder.stack)-1].UID
				m.folder.loading = true
				m.folder.loadErr = ""
				cmds = append(cmds, loadChildren(m.folder.client, parent))
			}
		case "enter":
			// Зайти внутрь space/folder, либо ничего для document.
			if it, ok := m.folder.list.SelectedItem().(treeItem); ok {
				if it.entityType == kaiten.EntityTypeSpace || it.entityType == kaiten.EntityTypeDocumentGroup {
					m.folder.stack = append(m.folder.stack, breadcrumb{UID: it.uid, Title: it.title})
					m.folder.loading = true
					m.folder.loadErr = ""
					cmds = append(cmds, loadChildren(m.folder.client, it.uid))
				}
			}
		case " ", "s":
			// Выбрать текущий уровень для синхронизации.
			if len(m.folder.stack) <= 1 {
				m.folder.loadErr = "выберите папку или пространство (нельзя синхронизировать корень)"
				return m, nil
			}
			cur := m.folder.stack[len(m.folder.stack)-1]
			m.result.RootUID = cur.UID
			m.stage = stageDone
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.folder.list.SetSize(msg.Width-4, msg.Height-8)
	case childrenOKMsg:
		m.folder.loading = false
		m.folder.list.SetItems(msg.entries)
	case childrenErrMsg:
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
	case stageFolderPick:
		return m.viewFolderPick()
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

func (m *model) viewFolderPick() string {
	var b strings.Builder
	// Breadcrumbs.
	crumbs := make([]string, 0, len(m.folder.stack))
	for _, c := range m.folder.stack {
		crumbs = append(crumbs, c.Title)
	}
	b.WriteString(titleStyle.Render("📂 " + strings.Join(crumbs, " / ")))
	b.WriteString("\n")
	if m.folder.loading {
		b.WriteString(m.folder.spinner.View() + " загрузка…\n")
	}
	if m.folder.loadErr != "" {
		b.WriteString(errStyle.Render("ошибка: "+m.folder.loadErr) + "\n")
	}
	b.WriteString(m.folder.list.View())
	b.WriteString("\n")
	b.WriteString(hintStyle.Render("enter — войти в папку/пространство, space или s — выбрать ТЕКУЩИЙ уровень для синхронизации, ← / backspace — назад, esc — выход"))
	return b.String()
}

// Result возвращает результат после Quit.
func (m *model) Result() Result { return m.result }

// ---------- list.Item ----------

// treeItem — элемент списка: space / folder / document с UID и типом.
type treeItem struct {
	uid        string
	title      string
	entityType string
}

func (t treeItem) Title() string {
	icon := "•"
	switch t.entityType {
	case kaiten.EntityTypeSpace:
		icon = "🌐"
	case kaiten.EntityTypeDocumentGroup:
		icon = "📁"
	case kaiten.EntityTypeDocument:
		icon = "📄"
	}
	return icon + " " + t.title
}
func (t treeItem) Description() string {
	switch t.entityType {
	case kaiten.EntityTypeSpace:
		return "пространство"
	case kaiten.EntityTypeDocumentGroup:
		return "папка"
	case kaiten.EntityTypeDocument:
		return "документ"
	}
	return t.entityType
}
func (t treeItem) FilterValue() string { return t.title }

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
