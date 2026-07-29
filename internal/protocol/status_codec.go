package protocol

import (
	"fmt"
	"unicode/utf8"
)

func EncodeClientStatus(dst []byte, msg ClientStatus) ([]byte, error) {
	if msg.Revision == 0 {
		return nil, fmt.Errorf("encode ClientStatus: zero revision")
	}
	if uint64(len(msg.Windows)) > MaxStatusWindows {
		return nil, fmt.Errorf("encode ClientStatus: window count %d exceeds max %d", len(msg.Windows), MaxStatusWindows)
	}
	if msg.Kind < ClientStatusNormal || msg.Kind > ClientStatusMessage {
		return nil, fmt.Errorf("encode ClientStatus: invalid presentation kind %d", msg.Kind)
	}
	for name, value := range map[string]string{
		"session name":   msg.SessionName,
		"hostname":       msg.ServerHostname,
		"server home":    msg.ServerHome,
		"root":           msg.Root,
		"prompt label":   msg.Prompt.Label,
		"prompt initial": msg.Prompt.Initial,
		"message":        msg.Message.Text,
	} {
		if err := validateClientStatusString(name, value); err != nil {
			return nil, err
		}
	}
	if msg.Kind == ClientStatusPrompt {
		if msg.Prompt.PromptID == 0 {
			return nil, fmt.Errorf("encode ClientStatus: zero prompt ID")
		}
		if msg.Prompt.Mode < ClientStatusPromptText || msg.Prompt.Mode > ClientStatusPromptConfirm {
			return nil, fmt.Errorf("encode ClientStatus: invalid prompt mode %d", msg.Prompt.Mode)
		}
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.Revision)
	w.Uvarint(msg.SessionID)
	w.String(msg.SessionName)
	w.String(msg.ServerHostname)
	w.String(msg.ServerHome)
	w.String(msg.Root)
	w.Uvarint(uint64(len(msg.Windows)))
	for _, window := range msg.Windows {
		if window.Index < 0 {
			return nil, fmt.Errorf("encode ClientStatus: negative window index")
		}
		if err := validateClientStatusString("window title", window.Title); err != nil {
			return nil, err
		}
		w.Uvarint(window.WindowID)
		w.Uvarint(uint64(window.Index))
		w.String(window.Title)
		w.Bool(window.Active)
		w.Bool(window.Zoomed)
	}
	w.Byte(byte(msg.Kind))
	w.Uvarint(msg.Prompt.PromptID)
	w.Byte(byte(msg.Prompt.Mode))
	w.String(msg.Prompt.Label)
	w.String(msg.Prompt.Initial)
	w.Uvarint(msg.Message.ID)
	w.String(msg.Message.Text)
	if len(w.Buf)-len(dst) > DefaultMaxFrameSize {
		return nil, fmt.Errorf("encode ClientStatus: payload exceeds max frame size")
	}
	return w.Buf, nil
}

func validateClientStatusString(name, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("encode ClientStatus: %s is not valid UTF-8", name)
	}
	if uint64(len(value)) > MaxStringLen {
		return fmt.Errorf("encode ClientStatus: %s length %d exceeds max %d", name, len(value), MaxStringLen)
	}
	return nil
}

func DecodeClientStatus(payload []byte) (ClientStatus, error) {
	r := PayloadReader{Data: payload}
	var msg ClientStatus
	var err error
	if msg.Revision, err = r.Uvarint(); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus revision: %w", err)
	}
	if msg.Revision == 0 {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus revision: zero revision")
	}
	if msg.SessionID, err = r.Uvarint(); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus session: %w", err)
	}
	if msg.SessionName, err = r.String(MaxStringLen); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus session name: %w", err)
	}
	if msg.ServerHostname, err = r.String(MaxStringLen); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus hostname: %w", err)
	}
	if msg.ServerHome, err = r.String(MaxStringLen); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus home: %w", err)
	}
	if msg.Root, err = r.String(MaxStringLen); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus root: %w", err)
	}
	count, err := readCount(&r, MaxStatusWindows)
	if err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus windows: %w", err)
	}
	msg.Windows = make([]ClientStatusWindow, 0, count)
	for i := 0; i < count; i++ {
		var window ClientStatusWindow
		index, readErr := uint64(0), error(nil)
		if window.WindowID, readErr = r.Uvarint(); readErr == nil {
			index, readErr = r.Uvarint()
		}
		if readErr == nil {
			window.Title, readErr = r.String(MaxStringLen)
		}
		if readErr == nil {
			window.Active, readErr = r.Bool()
		}
		if readErr == nil {
			window.Zoomed, readErr = r.Bool()
		}
		if readErr != nil {
			return ClientStatus{}, fmt.Errorf("decode ClientStatus window %d: %w", i, readErr)
		}
		if index > uint64(^uint(0)>>1) {
			return ClientStatus{}, fmt.Errorf("decode ClientStatus window %d: index overflows int", i)
		}
		window.Index = int(index)
		msg.Windows = append(msg.Windows, window)
	}
	kind, err := r.Byte()
	if err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus presentation: %w", err)
	}
	msg.Kind = ClientStatusPresentationKind(kind)
	if msg.Prompt.PromptID, err = r.Uvarint(); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus prompt ID: %w", err)
	}
	mode, err := r.Byte()
	if err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus prompt mode: %w", err)
	}
	msg.Prompt.Mode = ClientStatusPromptMode(mode)
	if msg.Prompt.Label, err = r.String(MaxStringLen); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus prompt label: %w", err)
	}
	if msg.Prompt.Initial, err = r.String(MaxStringLen); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus prompt initial: %w", err)
	}
	if msg.Message.ID, err = r.Uvarint(); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus message ID: %w", err)
	}
	if msg.Message.Text, err = r.String(MaxStringLen); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus message: %w", err)
	}
	if err := r.Done(); err != nil {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus: %w", err)
	}
	if msg.Kind < ClientStatusNormal || msg.Kind > ClientStatusMessage {
		return ClientStatus{}, fmt.Errorf("decode ClientStatus: invalid presentation kind %d", msg.Kind)
	}
	if msg.Kind == ClientStatusPrompt {
		if msg.Prompt.PromptID == 0 {
			return ClientStatus{}, fmt.Errorf("decode ClientStatus: zero prompt ID")
		}
		if msg.Prompt.Mode < ClientStatusPromptText || msg.Prompt.Mode > ClientStatusPromptConfirm {
			return ClientStatus{}, fmt.Errorf("decode ClientStatus: invalid prompt mode %d", msg.Prompt.Mode)
		}
	}
	return msg, nil
}
