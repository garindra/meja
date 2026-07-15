package client

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

const (
	incomingBurstWindow        = 50 * time.Millisecond
	renderDiagnosticBufferSize = 4096
)

type renderDiagnosticKind uint8

const (
	renderDiagnosticCommand renderDiagnosticKind = iota + 1
	renderDiagnosticRedraw
	renderDiagnosticStop
)

type renderDiagnosticReport struct {
	kind       renderDiagnosticKind
	slot       uint8
	opcode     protocol.DisplayOpcode
	wireBytes  uint64
	textBytes  uint64
	styleID    uint32
	style      protocol.Style
	reason     string
	writeBytes int
	ack        chan struct{}
}

type renderDiagnostics struct {
	reports   chan renderDiagnosticReport
	done      chan struct{}
	closeOnce sync.Once
}

type renderDiagnosticState struct {
	writer          io.Writer
	burstStarted    time.Time
	wireBytes       uint64
	textBytes       uint64
	commandCount    uint64
	messageTypeHits map[protocol.DisplayOpcode]uint64
	writeStyleHits  map[renderStyleKey]uint64
	installedStyles map[renderStyleKey]protocol.Style
	redrawRequests  uint64
	redrawWrites    uint64
}

type renderStyleKey struct {
	slot uint8
	id   uint32
}

func newRenderDiagnostics(writer io.Writer) *renderDiagnostics {
	if writer == nil {
		return nil
	}
	diagnostics := &renderDiagnostics{
		reports: make(chan renderDiagnosticReport, renderDiagnosticBufferSize),
		done:    make(chan struct{}),
	}
	go diagnostics.run(writer)
	return diagnostics
}

func (d *renderDiagnostics) reportCommand(slot uint8, command protocol.DisplayCommand, wireBytes uint64) {
	if d == nil {
		return
	}
	report := renderDiagnosticReport{
		kind:      renderDiagnosticCommand,
		slot:      slot,
		opcode:    command.Opcode,
		wireBytes: wireBytes,
		textBytes: uint64(len(command.Text)),
	}
	if command.Opcode == protocol.DisplayOpcodeStyleInstall || command.Opcode == protocol.DisplayOpcodeSetWriteStyle {
		report.styleID = command.StyleID
		report.style = command.Style
	}
	d.report(report)
}

func (d *renderDiagnostics) reportRedraw(reason string, writeBytes int) {
	if d == nil {
		return
	}
	d.report(renderDiagnosticReport{kind: renderDiagnosticRedraw, reason: reason, writeBytes: writeBytes})
}

func (d *renderDiagnostics) report(report renderDiagnosticReport) {
	select {
	case d.reports <- report:
	case <-d.done:
	}
}

func (d *renderDiagnostics) close() {
	if d == nil {
		return
	}
	d.closeOnce.Do(func() {
		ack := make(chan struct{})
		select {
		case d.reports <- renderDiagnosticReport{kind: renderDiagnosticStop, ack: ack}:
			<-ack
		case <-d.done:
		}
	})
}

func (d *renderDiagnostics) run(writer io.Writer) {
	state := renderDiagnosticState{writer: writer, installedStyles: make(map[renderStyleKey]protocol.Style)}
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case report := <-d.reports:
			switch report.kind {
			case renderDiagnosticCommand:
				if state.burstStarted.IsZero() {
					state.startBurst()
					timer = time.NewTimer(incomingBurstWindow)
					timerC = timer.C
				}
				state.recordCommand(report)
			case renderDiagnosticRedraw:
				state.recordRedraw(report)
			case renderDiagnosticStop:
				if timer != nil {
					timer.Stop()
				}
				state.flushBurst()
				close(d.done)
				close(report.ack)
				return
			}
		case <-timerC:
			state.flushBurst()
			timer, timerC = nil, nil
		}
	}
}

func (s *renderDiagnosticState) startBurst() {
	s.burstStarted = time.Now()
	s.messageTypeHits = make(map[protocol.DisplayOpcode]uint64)
	s.writeStyleHits = make(map[renderStyleKey]uint64)
}

func (s *renderDiagnosticState) recordCommand(report renderDiagnosticReport) {
	s.wireBytes += report.wireBytes
	s.textBytes += report.textBytes
	s.commandCount++
	s.messageTypeHits[report.opcode]++
	key := renderStyleKey{slot: report.slot, id: report.styleID}
	if report.opcode == protocol.DisplayOpcodeStyleInstall {
		s.installedStyles[key] = report.style
	}
	if report.opcode == protocol.DisplayOpcodeSetWriteStyle {
		s.writeStyleHits[key]++
	}
}

func (s *renderDiagnosticState) recordRedraw(report renderDiagnosticReport) {
	s.redrawRequests++
	s.logf("redraw request #%d: %s", s.redrawRequests, report.reason)
	s.redrawWrites++
	s.logf("redraw write #%d bytes=%d", s.redrawWrites, report.writeBytes)
}

func (s *renderDiagnosticState) flushBurst() {
	if s.burstStarted.IsZero() {
		return
	}
	s.logf(
		"incoming burst at=%s window=%s elapsed=%s wire_bytes=%d text_bytes=%d commands=%d types=%s write_styles=%s",
		time.Now().Format(time.RFC3339Nano),
		incomingBurstWindow,
		time.Since(s.burstStarted).Round(time.Millisecond),
		s.wireBytes,
		s.textBytes,
		s.commandCount,
		formatIncomingRenderTypes(s.messageTypeHits),
		formatIncomingWriteStyles(s.writeStyleHits, s.installedStyles),
	)
	s.burstStarted = time.Time{}
	s.wireBytes = 0
	s.textBytes = 0
	s.commandCount = 0
	s.messageTypeHits = nil
	s.writeStyleHits = nil
}

func (s *renderDiagnosticState) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.writer, "meja render: "+format+"\n", args...)
}

func formatIncomingWriteStyles(hits map[renderStyleKey]uint64, styles map[renderStyleKey]protocol.Style) string {
	if len(hits) == 0 {
		return "none"
	}
	keys := make([]renderStyleKey, 0, len(hits))
	for key := range hits {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].slot != keys[j].slot {
			return keys[i].slot < keys[j].slot
		}
		return keys[i].id < keys[j].id
	})
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		style, ok := styles[key]
		description := "unknown"
		if ok {
			description = formatRenderStyle(style)
		}
		parts = append(parts, fmt.Sprintf("slot%d/id%d:%d{%s}", key.slot, key.id, hits[key], description))
	}
	return strings.Join(parts, ",")
}

func formatRenderStyle(style protocol.Style) string {
	flags := make([]string, 0, 7)
	if style.Bold {
		flags = append(flags, "bold")
	}
	if style.Dim {
		flags = append(flags, "dim")
	}
	if style.Blink {
		flags = append(flags, "blink")
	}
	if style.Italic {
		flags = append(flags, "italic")
	}
	if style.Underline {
		flags = append(flags, "underline")
	}
	if style.Reverse {
		flags = append(flags, "reverse")
	}
	if style.Invisible {
		flags = append(flags, "invisible")
	}
	if len(flags) == 0 {
		flags = append(flags, "plain")
	}
	return fmt.Sprintf("%s,fg=%s,bg=%s", strings.Join(flags, "+"), formatRenderColor(style.FG), formatRenderColor(style.BG))
}

func formatRenderColor(color protocol.Color) string {
	switch color.Mode {
	case "indexed":
		return fmt.Sprintf("idx%d", color.Index)
	case "rgb":
		return fmt.Sprintf("#%02x%02x%02x", color.R, color.G, color.B)
	default:
		return "default"
	}
}

func formatIncomingRenderTypes(types map[protocol.DisplayOpcode]uint64) string {
	if len(types) == 0 {
		return "none"
	}
	keys := make([]protocol.DisplayOpcode, 0, len(types))
	for msgType := range types {
		keys = append(keys, msgType)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	parts := make([]string, 0, len(keys))
	for _, msgType := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", incomingRenderOpcodeName(msgType), types[msgType]))
	}
	return strings.Join(parts, ",")
}

func incomingRenderOpcodeName(opcode protocol.DisplayOpcode) string {
	switch opcode {
	case protocol.DisplayOpcodeNoop:
		return "Noop"
	case protocol.DisplayOpcodeRelayoutBarrier:
		return "RelayoutBarrier"
	case protocol.DisplayOpcodeStyleInstall:
		return "StyleInstall"
	case protocol.DisplayOpcodeSetWritePosition:
		return "SetWritePosition"
	case protocol.DisplayOpcodeSetWriteStyle:
		return "SetWriteStyle"
	case protocol.DisplayOpcodeWriteText:
		return "WriteText"
	case protocol.DisplayOpcodeWriteTextUTF8:
		return "WriteTextUTF8"
	case protocol.DisplayOpcodeWriteTextUTF8Default:
		return "WriteTextUTF8Default"
	case protocol.DisplayOpcodeFill:
		return "Fill"
	case protocol.DisplayOpcodeCursorUpdate:
		return "CursorUpdate"
	case protocol.DisplayOpcodeScroll:
		return "Scroll"
	case protocol.DisplayOpcodePresent:
		return "Present"
	default:
		return fmt.Sprintf("Opcode0x%02x", byte(opcode))
	}
}
