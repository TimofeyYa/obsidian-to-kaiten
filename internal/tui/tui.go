// Package tui — экраны Bubble Tea для интерактивного режима.
// Три этапа: ввод URL+токена → выбор пространства → прогресс синка.
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
	SpaceID int
	Aborted bool
}

// ---------- Стили ----------

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4D4D"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#22C55E"))
	hintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#888"))
)

// ---------- Stage 1: вход (URL + Token) ----------

type stage int

const (
	stageLogin stage = iota
	stageSpacePick
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

type model struct {
	stage  stage
	login  loginModel
	picker list.Model
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

type checkOKMsg struct {
	user   *kaiten.User
	spaces []kaiten.Space
}
type checkErrMsg struct{ err string }

// checkCredentials — async задача для проверки токена.
func checkCredentials(baseURL, token string) tea.Cmd {
	return func() tea.Msg {
		c := kaiten.New(baseURL, token)
		ctx := context.Background()
		u, err := c.GetCurrentUser(ctx)
		if err != nil {
			return checkErrMsg{err: err.Error()}
		}
		sps, err := c.ListSpaces(ctx)
		if err != nil {
			return checkErrMsg{err: "spaces: " + err.Error()}
		}
		return checkOKMsg{user: u, spaces: sps}
	}
}

// ---------- Update ----------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m.stage {
	case stageLogin:
		return m.updateLogin(msg)
	case stageSpacePick:
		return m.updateSpacePick(msg)
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
				cmds = append(cmds, checkCredentials(url, tok), m.login.spinner.Tick)
			}
		}
	case spinner.TickMsg:
		var c tea.Cmd
		m.login.spinner, c = m.login.spinner.Update(msg)
		if m.login.checking {
			cmds = append(cmds, c)
		}
	case checkOKMsg:
		m.login.checking = false
		m.login.user = msg.user
		m.result.BaseURL = strings.TrimRight(m.login.inputs[0].Value(), "/")
		m.result.Token = m.login.inputs[1].Value()
		// Готовим list для выбора пространства.
		items := make([]list.Item, 0, len(msg.spaces))
		for _, s := range msg.spaces {
			items = append(items, spaceItem{id: s.ID, title: s.Title})
		}
		del := list.NewDefaultDelegate()
		l := list.New(items, del, 70, 18)
		l.Title = "Выберите корневое пространство Kaiten"
		l.SetShowStatusBar(false)
		m.picker = l
		m.stage = stageSpacePick
		return m, nil
	case checkErrMsg:
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

func (m *model) updateSpacePick(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.result.Aborted = true
			return m, tea.Quit
		case "enter":
			if it, ok := m.picker.SelectedItem().(spaceItem); ok {
				m.result.SpaceID = it.id
				m.stage = stageDone
				return m, tea.Quit
			}
		}
	case tea.WindowSizeMsg:
		m.picker.SetSize(msg.Width-4, msg.Height-6)
	}
	var c tea.Cmd
	m.picker, c = m.picker.Update(msg)
	return m, c
}

// ---------- View ----------

func (m *model) View() string {
	switch m.stage {
	case stageLogin:
		return m.viewLogin()
	case stageSpacePick:
		return "\n" + m.picker.View() + "\n" + hintStyle.Render("enter — выбрать, esc — выход")
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

// Result возвращает результат после Quit.
func (m *model) Result() Result { return m.result }

// ---------- list.Item ----------

type spaceItem struct {
	id    int
	title string
}

func (s spaceItem) Title() string       { return s.title }
func (s spaceItem) Description() string { return fmt.Sprintf("id=%d", s.id) }
func (s spaceItem) FilterValue() string { return s.title }

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
