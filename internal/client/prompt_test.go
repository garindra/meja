package client

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func TestClientPromptDraftEditsUTF8AndFragmentedDeleteLocally(t *testing.T) {
	draft := newClientPromptDraft(protocol.ClientStatusPromptState{
		PromptID: 1, Mode: protocol.ClientStatusPromptText, Label: ":", Initial: "猫a",
	})
	encoded := []byte("é")
	if outcome := draft.consume(encoded[:1], false); !outcome.handled || outcome.changed {
		t.Fatalf("first UTF-8 fragment outcome = %#v", outcome)
	}
	if outcome := draft.consume(encoded[1:], false); !outcome.changed {
		t.Fatalf("completed UTF-8 outcome = %#v", outcome)
	}
	if got := string(draft.text); got != "猫aé" {
		t.Fatalf("draft after UTF-8 input = %q", got)
	}

	if outcome := draft.consume([]byte("\x1b[D"), false); !outcome.changed {
		t.Fatalf("left outcome = %#v", outcome)
	}
	sequence := []byte("\x1b[3~")
	for i, fragment := range [][]byte{sequence[:2], sequence[2:3], sequence[3:]} {
		outcome := draft.consume(fragment, false)
		if i < 2 && outcome.changed {
			t.Fatalf("delete fragment %d changed draft: %#v", i, outcome)
		}
		if i == 2 && !outcome.changed {
			t.Fatalf("completed delete did not change draft: %#v", outcome)
		}
	}
	if got := string(draft.text); got != "猫a" {
		t.Fatalf("draft after delete = %q, want 猫a", got)
	}

	outcome := draft.consume([]byte{'\r'}, false)
	if outcome.result == nil || !outcome.result.Submitted ||
		outcome.result.PromptID != 1 || outcome.result.Text != "猫a" {
		t.Fatalf("submit outcome = %#v", outcome)
	}
}

func TestClientPromptDraftHandlesBracketedPasteAndConfirmation(t *testing.T) {
	draft := newClientPromptDraft(protocol.ClientStatusPromptState{
		PromptID: 2, Mode: protocol.ClientStatusPromptText,
	})
	outcome := draft.consume([]byte("\x1b[200~hello\n世界\x1b[201~"), false)
	if !outcome.changed || string(draft.text) != "hello 世界" {
		t.Fatalf("paste outcome=%#v draft=%q", outcome, draft.text)
	}

	confirm := newClientPromptDraft(protocol.ClientStatusPromptState{
		PromptID: 3, Mode: protocol.ClientStatusPromptConfirm,
	})
	outcome = confirm.consume([]byte("Y"), false)
	if outcome.result == nil || !outcome.result.Submitted || outcome.result.Text != "y" {
		t.Fatalf("confirmation outcome = %#v", outcome)
	}
}

func TestClientStatusPreservesDraftForSamePromptID(t *testing.T) {
	state := newScanoutState(false)
	state.cols, state.rows = 40, 4
	first := protocol.ClientStatus{
		Revision: 1, Kind: protocol.ClientStatusPrompt,
		Prompt: protocol.ClientStatusPromptState{
			PromptID: 7, Mode: protocol.ClientStatusPromptText, Label: "first: ", Initial: "a",
		},
	}
	if _, err := state.acceptStatus(first); err != nil {
		t.Fatal(err)
	}
	if outcome := state.acceptPromptInput([]byte("b"), false); !outcome.changed {
		t.Fatalf("local input outcome = %#v", outcome)
	}
	second := first
	second.Revision = 2
	second.Prompt.Label = "updated: "
	second.Prompt.Initial = "server-does-not-own-the-draft"
	if _, err := state.acceptStatus(second); err != nil {
		t.Fatal(err)
	}
	if got := string(state.prompt.text); got != "ab" {
		t.Fatalf("same-ID status replaced local draft with %q", got)
	}

	second.Revision = 3
	second.Prompt.PromptID = 8
	second.Prompt.Initial = "new"
	if _, err := state.acceptStatus(second); err != nil {
		t.Fatal(err)
	}
	if got := string(state.prompt.text); got != "new" {
		t.Fatalf("new prompt ID retained old draft %q", got)
	}
}

func TestPromptInputRoutingIgnoresStaleStatusRevision(t *testing.T) {
	ui := &runtimeState{}
	ui.updatePromptInputStatus(8, true)
	ui.updatePromptInputStatus(7, false)
	if status := ui.promptInputStatus.Load(); status == nil || status.revision != 8 || !status.active {
		t.Fatalf("stale status changed prompt input routing: %#v", status)
	}
	ui.updatePromptInputStatus(9, false)
	if status := ui.promptInputStatus.Load(); status == nil || status.revision != 9 || status.active {
		t.Fatalf("new status did not clear prompt input routing: %#v", status)
	}
}

func TestForwardInputSendsOnlyPromptResultAfterLocalEditing(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	frames := make(chan protocol.Frame, 4)
	reads := make(chan terminalInputRead, 1)
	errs := make(chan error, 1)
	var control atomic.Pointer[controlDestination]
	control.Store(&controlDestination{frames: frames, done: ctx.Done()})
	ui := &runtimeState{
		stdout: &lockedBuffer{}, events: make(chan renderEvent, 16),
		renderDone: make(chan struct{}),
	}
	go ui.renderLoop(ctx, errs)
	ui.emit(sizeEvent{cols: 40, rows: 4})
	ui.emit(statusEvent{status: protocol.ClientStatus{
		Revision: 1, Kind: protocol.ClientStatusPrompt,
		Prompt: protocol.ClientStatusPromptState{
			PromptID: 9, Mode: protocol.ClientStatusPromptText, Label: ":", Initial: "a",
		},
	}})
	ui.sync(ctx)
	ui.promptInputStatus.Store(&promptInputStatus{revision: 1, active: true})
	go forwardInputReads(ctx, reads, &control, ui, errs, cancel, time.Hour)

	reads <- terminalInputRead{data: []byte("b\r")}
	close(reads)
	select {
	case err := <-errs:
		t.Fatalf("prompt input forwarding error = %v", err)
	case frame := <-frames:
		if frame.Type != protocol.MsgFrontendPromptResult {
			t.Fatalf("prompt frame type = %d, want FRONTEND_PROMPT_RESULT", frame.Type)
		}
		result, err := protocol.DecodeFrontendPromptResult(frame.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if result.PromptID != 9 || !result.Submitted || result.Text != "ab" {
			t.Fatalf("prompt result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("local prompt editing did not produce a result")
	}
	select {
	case frame := <-frames:
		t.Fatalf("prompt keystrokes also produced a control frame: %#v", frame)
	default:
	}
}
