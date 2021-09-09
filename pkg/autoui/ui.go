package autoui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/pkg/errors"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"golang.org/x/term"
)

const columnWidth = 50

// Style definitions.
var (
	subtle  = lipgloss.AdaptiveColor{Light: "#D9DCCF", Dark: "#383838"}
	special = lipgloss.AdaptiveColor{Light: "#43BF6D", Dark: "#73F59F"}

	list = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder(), false, true, false, false).
		BorderForeground(subtle).
		MarginRight(2).
		Height(8).
		Width(columnWidth + 1)

	listHeader = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderBottom(true).
			BorderForeground(subtle).
			MarginRight(2).
			Render

	listItem = lipgloss.NewStyle().PaddingLeft(2).Render

	checkMark = lipgloss.NewStyle().SetString("✓").
			Foreground(special).
			PaddingRight(1).
			String()

	listDone = func(s string) string {
		return checkMark + lipgloss.NewStyle().
			Strikethrough(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#969B86", Dark: "#696969"}).
			Render(s)
	}

	docStyle = lipgloss.NewStyle().Padding(1, 2, 1, 2)
)

// watchForLogMessages forwards any log messages to the `Update` method
func watchForLogMessages(msg chan logMessage) tea.Cmd {
	return func() tea.Msg {
		return <-msg
	}
}

// watchForEvents forwards any engine events to the `Update` method
func watchForEvents(event chan events.EngineEvent) tea.Cmd {
	return func() tea.Msg {
		return <-event
	}
}

type logMessage struct {
	msg string
}

// model is the struct that holds the state for this program
type model struct {
	eventChannel      chan events.EngineEvent // where we'll receive engine events
	logChannel        chan logMessage         // where we'll receive log messages
	spinner           spinner.Model
	mainFn            func(ctx *RunContext) error
	quitting          bool
	currentMessage    string
	updatesInProgress map[string]string // resources with updates in progress
	updatesComplete   map[string]string // resources with updates completed
}

// Init runs any IO needed at the initialization of the program
func (m model) Init() tea.Cmd {
	return tea.Batch(
		watchForLogMessages(m.logChannel),
		func() tea.Msg {
			err := m.mainFn(&RunContext{
				Context: context.Background(),
				Log: func(msg string, a ...interface{}) {
					m.logChannel <- logMessage{fmt.Sprintf(msg, a...)}
				},
				Events: m.eventChannel,
			})
			if err != nil {
				return logMessage{msg: fmt.Sprintf("Error: %v", err)}
			}
			return logMessage{msg: "Success"}
		},
		watchForEvents(m.eventChannel),
		spinner.Tick,
	)
}

// Update acts on any events and updates state (model) accordingly
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case events.EngineEvent:
		if msg.ResourcePreEvent != nil {
			m.updatesInProgress[msg.ResourcePreEvent.Metadata.URN] = msg.ResourcePreEvent.Metadata.Type
		}
		if msg.ResOutputsEvent != nil {
			urn := msg.ResOutputsEvent.Metadata.URN
			m.updatesComplete[urn] = msg.ResOutputsEvent.Metadata.Type
			delete(m.updatesInProgress, urn)
		}
		return m, watchForEvents(m.eventChannel) // wait for next event
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.KeyMsg:
		m.quitting = true
		return m, tea.Quit
	case logMessage:
		if msg.msg == "Success" {
			m.currentMessage = "Succeeded!"
			return m, tea.Quit
		}
		m.currentMessage = msg.msg
		return m, watchForLogMessages(m.logChannel)
	default:
		return m, nil
	}
}

// View displays the state in the terminal
func (m model) View() string {
	var inProgVals []string
	var completedVals []string
	doc := strings.Builder{}
	if len(m.updatesInProgress) > 0 || len(m.updatesComplete) > 0 {
		for _, v := range m.updatesInProgress {
			inProgVals = append(inProgVals, listItem(v))
		}
		sort.Strings(inProgVals)
		for _, v := range m.updatesComplete {
			completedVals = append(completedVals, listDone(v))
		}
		sort.Strings(completedVals)

		inProgVals = append([]string{listHeader("Updates in progress")}, inProgVals...)
		completedVals = append([]string{listHeader("Updates completed")}, completedVals...)
		lists := lipgloss.JoinHorizontal(lipgloss.Top,
			list.Render(
				lipgloss.JoinVertical(lipgloss.Left,
					inProgVals...,
				),
			),
			list.Copy().Width(columnWidth).Render(
				lipgloss.JoinVertical(lipgloss.Left,
					completedVals...,
				),
			),
		)
		doc.WriteString("\n")
		doc.WriteString(lists)
	}

	physicalWidth, _, _ := term.GetSize(int(os.Stdout.Fd()))
	if physicalWidth > 0 {
		docStyle = docStyle.MaxWidth(physicalWidth)
	}

	s := fmt.Sprintf("\n%sCurrent step: %s%s\n", m.spinner.View(), m.currentMessage, docStyle.Render(doc.String()))
	if m.quitting {
		s += "\n"
	}
	return s
}

func runInner(mainFn func(ctx *RunContext) error) error {
	s := spinner.NewModel()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	p := tea.NewProgram(model{
		logChannel:        make(chan logMessage),
		eventChannel:      make(chan events.EngineEvent),
		mainFn:            mainFn,
		spinner:           s,
		updatesInProgress: map[string]string{},
		updatesComplete:   map[string]string{},
	})

	return p.Start()
}

func Run(stack auto.Stack, destroy bool) (auto.OutputMap, error) {
	var outputs auto.OutputMap
	err := runInner(func(rc *RunContext) error {
		// Create our Pulumi program and workspace, and prepare to either deploy or destroy.
		if destroy {
			rc.Log("Running destroy...")

			// Destroy the entire stack!
			_, err := stack.Destroy(rc.Context, optdestroy.EventStreams(rc.Events))
			if err != nil {
				return errors.Wrapf(err, "destroying stack")
			}
			rc.Log("Stack successfully destroyed")
		} else {
			// Otherwise, run an update to execute whatever CRUDs are implied by the IaC diffs.
			rc.Log("Running update...")
			res, err := stack.Up(rc.Context, optup.EventStreams(rc.Events))
			if err != nil {
				return errors.Wrapf(err, "updating stack")
			}
			outputs = res.Outputs
			rc.Log("Update succeeded!")
		}
		return nil
	})
	return outputs, err
}

type RunContext struct {
	Log     func(msg string, a ...interface{})
	Events  chan<- events.EngineEvent
	Context context.Context
}
