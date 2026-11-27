package desktop

import (
	"context"
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

const waitingOnUserLabel = "Waiting on you"

// questionsView holds the cross-project list of agent questions that need a
// user answer. Each question closes when the user answers it here.
type questionsView struct {
	items []storage.TaskQuestion
	// cards keeps each open question's widgets so refreshes keep drafts,
	// focus and in-flight send state.
	cards       map[string]*questionCard
	seen        map[string]bool
	initialized bool
	loading     bool
	status      *widget.Label
	rows        *fyne.Container
	content     fyne.CanvasObject
}

func newQuestionsView() *questionsView {
	view := &questionsView{cards: map[string]*questionCard{}, seen: map[string]bool{}, status: widget.NewLabel("Loading questions…"), rows: container.NewVBox()}
	view.status.Wrapping = fyne.TextWrapWord
	view.content = container.NewBorder(container.NewVBox(browserSectionHeading("QUESTIONS"), view.status), nil, nil, nil, container.NewVScroll(view.rows))
	return view
}

// newQuestionIDs records the current open questions and returns the ones not
// seen before. The first load only seeds the seen set.
func (v *questionsView) newQuestionIDs(items []storage.TaskQuestion) []storage.TaskQuestion {
	var fresh []storage.TaskQuestion
	for _, item := range items {
		if !v.seen[item.ID] && v.initialized {
			fresh = append(fresh, item)
		}
		v.seen[item.ID] = true
	}
	v.initialized = true
	v.items = items
	return fresh
}

func questionsButtonLabel(count int) string {
	if count == 0 {
		return "Questions"
	}
	return fmt.Sprintf("Questions · %d", count)
}

func (b *Browser) refreshQuestions() {
	if b.questions == nil || b.questions.loading || b.client == nil {
		return
	}
	b.questions.loading = b.launch(func(ctx context.Context) {
		items, err := b.client.OpenTaskQuestions(ctx)
		b.dispatch(func() {
			b.questions.loading = false
			if b.closing {
				return
			}
			if err != nil {
				b.questions.status.SetText("Questions unavailable: " + err.Error())
				return
			}
			for _, question := range b.questions.newQuestionIDs(items) {
				if app := fyne.CurrentApp(); app != nil {
					app.SendNotification(fyne.NewNotification("Agent question: "+question.TaskTitle, question.ProjectIdent+" · Open Questions to answer"))
				}
			}
			b.applyQuestionsButton()
			if b.mainView == "Questions" {
				b.renderQuestions()
			}
		})
	})
}

func (b *Browser) applyQuestionsButton() {
	button := b.mainViewButtons["Questions"]
	if button == nil || b.questions == nil {
		return
	}
	count := len(b.questions.items)
	button.SetText(questionsButtonLabel(count))
	if count > 0 && b.mainView != "Questions" {
		button.Importance = widget.WarningImportance
	} else if b.mainView == "Questions" {
		button.Importance = widget.HighImportance
	} else {
		button.Importance = widget.MediumImportance
	}
	button.Refresh()
}

func (b *Browser) renderQuestions() {
	view := b.questions
	switch len(view.items) {
	case 0:
		view.status.SetText("No agent is waiting on you.")
	case 1:
		view.status.SetText("1 open question. Answering posts your reply as a task comment and resumes the agent.")
	default:
		view.status.SetText(fmt.Sprintf("%d open questions, newest first. Answering posts your reply as a task comment and resumes the agent.", len(view.items)))
	}
	cards := make(map[string]*questionCard, len(view.items))
	rows := make([]fyne.CanvasObject, 0, len(view.items))
	for _, item := range view.items {
		card := view.cards[item.ID]
		if card == nil {
			card = b.questionCard(item)
		}
		cards[item.ID] = card
		rows = append(rows, card.object)
	}
	view.cards = cards
	// Swap the slice instead of removing and re-adding children so unchanged
	// cards are never detached.
	view.rows.Objects = rows
	view.rows.Refresh()
}

type questionCard struct {
	object fyne.CanvasObject
	answer *widget.Entry
	send   func(string)
}

func (b *Browser) questionCard(item storage.TaskQuestion) *questionCard {
	heading := widget.NewLabelWithStyle(item.ProjectIdent+" · "+item.TaskTitle, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	heading.Truncation = fyne.TextTruncateEllipsis
	question := widget.NewLabel(item.Question)
	question.Wrapping = fyne.TextWrapWord
	answer := widget.NewMultiLineEntry()
	answer.SetPlaceHolder("Write an answer…")
	answer.Wrapping = fyne.TextWrapWord
	answer.SetMinRowsVisible(2)
	var controls []*widget.Button
	send := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		for _, control := range controls {
			control.Disable()
		}
		b.launch(func(ctx context.Context) {
			_, err := b.client.AnswerTaskQuestion(ctx, item.ProjectIdent, item.TaskID, item.ID, text)
			b.dispatch(func() {
				if err != nil {
					for _, control := range controls {
						control.Enable()
					}
					b.questions.status.SetText("Answer not sent: " + err.Error())
					return
				}
				b.refreshQuestions()
			})
		})
	}
	options := container.NewVBox()
	for _, option := range item.Options {
		choice := option
		button := widget.NewButton(choice, func() { send(choice) })
		button.Alignment = widget.ButtonAlignLeading
		controls = append(controls, button)
		options.Add(button)
	}
	reply := widget.NewButton("Send answer", func() { send(answer.Text) })
	reply.Importance = widget.HighImportance
	open := widget.NewButton("Open task", func() { b.openTaskFromProject(item.ProjectIdent, item.TaskID) })
	controls = append(controls, reply)
	body := container.NewVBox(heading, question, options, answer, container.NewHBox(reply, open))
	return &questionCard{object: container.NewVBox(container.NewStack(browserCardSurface(), container.NewPadded(body)), widget.NewSeparator()), answer: answer, send: send}
}
