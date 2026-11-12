package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AskUserQuestion — the structured question channel, after Claude
// Code's AskUserQuestion and Codex's tool/requestUserInput (the
// same concept under both names).
//
// The model calls the ask_user tool with 1–4 questions; each
// question is one multiple-choice question (2–4 options) plus an
// implicit free-text "Other". The user answers by picking options
// or typing their own answer, and the answers come back as one tool
// observation. This is for plan mode above all: the model asks what
// it needs to know instead of guessing, and the answer is a
// selection, not a free-form chat turn.
//
// The channel is a tool, not a permission: asking a question never
// needs approval, and the loop intercepts ask_user before dispatch
// (there is no executor tool of that name). Config.AskQuestion is
// the seam — the CLI reads answers from stdin, the RPC server sends
// a question_request event. Nil means nobody can be asked (a
// headless run): the model is told to proceed on its best
// judgement, and the turn continues.

const (
	// MaxQuestionsPerAsk is the per-call cap, like Claude's 4.
	MaxQuestionsPerAsk = 4
	// MaxOptionsPerQuestion is the per-question cap, like Claude's 4.
	MaxOptionsPerQuestion = 4
)

// QuestionOption is one selectable answer.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is one structured question.
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multi_select,omitempty"`
}

// QuestionRequest is the ask_user tool's arguments.
type QuestionRequest struct {
	Questions []Question `json:"questions"`
}

// QuestionAnswer is one question's answer: the selected option
// labels, and/or free text when the user chose "Other" or typed
// instead of picking.
type QuestionAnswer struct {
	Question int      `json:"question"`
	Selected []string `json:"selected,omitempty"`
	Custom   string   `json:"custom,omitempty"`
}

// QuestionAnswers is the reply to a QuestionRequest, in question
// order (the Question index in each answer is the index it answers).
type QuestionAnswers struct {
	Answers []QuestionAnswer `json:"answers"`
}

// askUserArgs is the raw tool-call shape, decoded before validation.
type askUserArgs struct {
	Questions []Question `json:"questions"`
}

// ParseAskUserArgs validates the ask_user tool's arguments: 1–4
// questions, each with a question and 2–4 uniquely labelled options.
func ParseAskUserArgs(argsJSON string) (QuestionRequest, error) {
	var raw askUserArgs
	if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
		return QuestionRequest{}, fmt.Errorf("ask_user: bad arguments: %w", err)
	}
	if len(raw.Questions) == 0 || len(raw.Questions) > MaxQuestionsPerAsk {
		return QuestionRequest{}, fmt.Errorf("ask_user: need 1-%d questions, got %d", MaxQuestionsPerAsk, len(raw.Questions))
	}
	for i, q := range raw.Questions {
		if strings.TrimSpace(q.Question) == "" {
			return QuestionRequest{}, fmt.Errorf("ask_user: question %d has no question text", i)
		}
		if len(q.Options) < 2 || len(q.Options) > MaxOptionsPerQuestion {
			return QuestionRequest{}, fmt.Errorf("ask_user: question %d needs 2-%d options, got %d", i, MaxOptionsPerQuestion, len(q.Options))
		}
		seen := map[string]bool{}
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" {
				return QuestionRequest{}, fmt.Errorf("ask_user: question %d has an option with no label", i)
			}
			if seen[o.Label] {
				return QuestionRequest{}, fmt.Errorf("ask_user: question %d repeats option label %q", i, o.Label)
			}
			seen[o.Label] = true
		}
	}
	return QuestionRequest{Questions: raw.Questions}, nil
}

// FormatAnswers renders the user's answers as the tool observation
// the model sees.
func FormatAnswers(req QuestionRequest, ans QuestionAnswers) string {
	var b strings.Builder
	b.WriteString("The user answered your questions:\n")
	for i, q := range req.Questions {
		b.WriteString(fmt.Sprintf("%d. %s\n", i+1, q.Question))
		var a *QuestionAnswer
		for j := range ans.Answers {
			if ans.Answers[j].Question == i {
				a = &ans.Answers[j]
				break
			}
		}
		if a == nil {
			b.WriteString("   (no answer)\n")
			continue
		}
		if len(a.Selected) > 0 {
			b.WriteString("   Selected: " + strings.Join(a.Selected, ", ") + "\n")
		}
		if strings.TrimSpace(a.Custom) != "" {
			b.WriteString("   Other: " + strings.TrimSpace(a.Custom) + "\n")
		}
		if len(a.Selected) == 0 && strings.TrimSpace(a.Custom) == "" {
			b.WriteString("   (no answer)\n")
		}
	}
	return b.String()
}
