package server

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	defaultPasteBufferLimit = 50
	maxPasteBufferBytes     = 8 << 20
)

type pasteBuffer struct {
	name      string
	data      []byte
	automatic bool
	created   uint64
}

// pasteBufferStore is daemon-wide, like tmux's buffer set. The mutex is
// required because command-socket handlers run concurrently with session
// actors that create buffers from copy mode.
type pasteBufferStore struct {
	mu          sync.Mutex
	buffers     map[string]pasteBuffer
	automatic   []string
	nextName    uint64
	nextCreated uint64
	limit       int
}

func (s *pasteBufferStore) ensureLocked() {
	if s.buffers == nil {
		s.buffers = make(map[string]pasteBuffer)
	}
	if s.limit <= 0 {
		s.limit = defaultPasteBufferLimit
	}
}

func (s *pasteBufferStore) addAutomatic(data []byte) (string, error) {
	return s.set("", false, data, false, "")
}

func (s *pasteBufferStore) set(name string, named bool, data []byte, appendData bool, rename string) (string, error) {
	if len(data) > maxPasteBufferBytes {
		return "", fmt.Errorf("paste buffer exceeds %d bytes", maxPasteBufferBytes)
	}
	if err := validatePasteBufferName(name); err != nil && (named || name != "") {
		return "", err
	}
	if err := validatePasteBufferName(rename); err != nil && rename != "" {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()

	target := name
	if !named && target == "" {
		if appendData || rename != "" {
			target = s.latestAutomaticLocked()
			if target == "" {
				return "", errors.New("no automatic paste buffer")
			}
		} else {
			target = s.nextAutomaticNameLocked()
		}
	}
	existing, exists := s.buffers[target]
	if appendData && !exists {
		return "", fmt.Errorf("no paste buffer %q", target)
	}
	if rename != "" {
		if !exists {
			return "", fmt.Errorf("no paste buffer %q", target)
		}
		if other, ok := s.buffers[rename]; ok && other.name != target {
			return "", fmt.Errorf("paste buffer %q already exists", rename)
		}
		delete(s.buffers, target)
		existing.name = rename
		if existing.automatic {
			for index, automaticName := range s.automatic {
				if automaticName == target {
					s.automatic[index] = rename
					break
				}
			}
		}
		target = rename
	}

	if appendData {
		if len(existing.data)+len(data) > maxPasteBufferBytes {
			return "", fmt.Errorf("paste buffer exceeds %d bytes", maxPasteBufferBytes)
		}
		data = append(append([]byte(nil), existing.data...), data...)
	} else {
		data = append([]byte(nil), data...)
	}
	if !exists {
		s.nextCreated++
		existing = pasteBuffer{name: target, automatic: !named, created: s.nextCreated}
	}
	existing.name = target
	existing.data = data
	if rename == "" && !exists {
		existing.automatic = !named
	}
	s.buffers[target] = existing
	if !exists && existing.automatic {
		s.automatic = append(s.automatic, target)
		s.trimAutomaticLocked()
	}
	return target, nil
}

func (s *pasteBufferStore) nextAutomaticNameLocked() string {
	for {
		s.nextName++
		name := fmt.Sprintf("buffer%04d", s.nextName)
		if _, exists := s.buffers[name]; !exists {
			return name
		}
	}
}

func (s *pasteBufferStore) latestAutomaticLocked() string {
	for index := len(s.automatic) - 1; index >= 0; index-- {
		name := s.automatic[index]
		if _, exists := s.buffers[name]; exists {
			return name
		}
	}
	return ""
}

func (s *pasteBufferStore) trimAutomaticLocked() {
	for len(s.automatic) > s.limit {
		oldest := s.automatic[0]
		s.automatic = s.automatic[1:]
		if buffer, exists := s.buffers[oldest]; exists && buffer.automatic {
			delete(s.buffers, oldest)
		}
	}
}

func (s *pasteBufferStore) get(name string) ([]byte, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	if name == "" {
		name = s.latestAutomaticLocked()
	}
	buffer, exists := s.buffers[name]
	if !exists {
		return nil, name, false
	}
	return append([]byte(nil), buffer.data...), name, true
}

func (s *pasteBufferStore) delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	if name == "" {
		name = s.latestAutomaticLocked()
	}
	if _, exists := s.buffers[name]; !exists {
		return fmt.Errorf("no paste buffer %q", name)
	}
	delete(s.buffers, name)
	return nil
}

func (s *pasteBufferStore) list() []pasteBuffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	result := make([]pasteBuffer, 0, len(s.buffers))
	for _, buffer := range s.buffers {
		buffer.data = append([]byte(nil), buffer.data...)
		result = append(result, buffer)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].created > result[j].created })
	return result
}

func validatePasteBufferName(name string) error {
	if name == "" {
		return nil
	}
	if !utf8.ValidString(name) || len(name) > 128 {
		return errors.New("paste buffer name must be valid UTF-8 and at most 128 bytes")
	}
	if strings.ContainsAny(name, "\r\n") {
		return errors.New("paste buffer name must not contain newlines")
	}
	return nil
}

func flagSetProvided(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			found = true
		}
	})
	return found
}

func handleSetBufferCommand(d *Daemon, _ CommandContext, args []string) (commandOutcome, error) {
	if d == nil {
		return commandOutcome{}, errors.New("set-buffer requires a running daemon")
	}
	fs := commandFlagSet("set-buffer")
	appendData := fs.Bool("a", false, "append")
	name := fs.String("b", "", "buffer name")
	newName := fs.String("n", "", "new buffer name")
	if err := fs.Parse(args); err != nil {
		return commandOutcome{}, err
	}
	if len(fs.Args()) != 1 {
		return commandOutcome{}, errors.New("set-buffer requires exactly one data argument")
	}
	hasName := flagSetProvided(fs, "b")
	if hasName {
		if err := validatePasteBufferName(*name); err != nil {
			return commandOutcome{}, err
		}
	}
	if _, err := d.pasteBuffers.set(*name, hasName, []byte(fs.Arg(0)), *appendData, *newName); err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{}, nil
}

func handleShowBufferCommand(d *Daemon, _ CommandContext, args []string) (commandOutcome, error) {
	if d == nil {
		return commandOutcome{}, errors.New("show-buffer requires a running daemon")
	}
	fs := commandFlagSet("show-buffer")
	name := fs.String("b", "", "buffer name")
	if err := fs.Parse(args); err != nil {
		return commandOutcome{}, err
	}
	if len(fs.Args()) != 0 {
		return commandOutcome{}, errors.New("show-buffer accepts no positional arguments")
	}
	data, resolved, exists := d.pasteBuffers.get(*name)
	if !exists {
		return commandOutcome{}, fmt.Errorf("no paste buffer %q", resolved)
	}
	return commandOutcome{Stdout: data}, nil
}

func handleListBuffersCommand(d *Daemon, _ CommandContext, args []string) (commandOutcome, error) {
	if d == nil {
		return commandOutcome{}, errors.New("list-buffers requires a running daemon")
	}
	if len(args) != 0 {
		return commandOutcome{}, errors.New("list-buffers accepts no arguments")
	}
	var output strings.Builder
	for _, buffer := range d.pasteBuffers.list() {
		fmt.Fprintf(&output, "%s: %d bytes\n", buffer.name, len(buffer.data))
	}
	data := []byte(output.String())
	return commandOutcome{Stdout: data}, nil
}

func handleDeleteBufferCommand(d *Daemon, _ CommandContext, args []string) (commandOutcome, error) {
	if d == nil {
		return commandOutcome{}, errors.New("delete-buffer requires a running daemon")
	}
	fs := commandFlagSet("delete-buffer")
	name := fs.String("b", "", "buffer name")
	if err := fs.Parse(args); err != nil {
		return commandOutcome{}, err
	}
	if len(fs.Args()) != 0 {
		return commandOutcome{}, errors.New("delete-buffer accepts no positional arguments")
	}
	if err := d.pasteBuffers.delete(*name); err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{}, nil
}

func commandWorkingDirectory(ctx CommandContext) string {
	return ctx.Caller.WorkingDirectory
}

func readPasteBufferFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPasteBufferBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPasteBufferBytes {
		return nil, fmt.Errorf("paste buffer exceeds %d bytes", maxPasteBufferBytes)
	}
	return data, nil
}

func handleLoadBufferCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	if d == nil {
		return commandOutcome{}, errors.New("load-buffer requires a running daemon")
	}
	fs := commandFlagSet("load-buffer")
	name := fs.String("b", "", "buffer name")
	if err := fs.Parse(args); err != nil {
		return commandOutcome{}, err
	}
	if len(fs.Args()) != 1 {
		return commandOutcome{}, errors.New("load-buffer requires a path")
	}
	if fs.Arg(0) == "-" {
		return commandOutcome{}, errors.New("load-buffer does not support stdin")
	}
	path, err := resolveCommandFilePath(fs.Arg(0), commandWorkingDirectory(ctx))
	if err != nil {
		return commandOutcome{}, err
	}
	data, err := readPasteBufferFile(path)
	if err != nil {
		return commandOutcome{}, err
	}
	_, err = d.pasteBuffers.set(*name, flagSetProvided(fs, "b"), data, false, "")
	return commandOutcome{}, err
}

func handleSaveBufferCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	if d == nil {
		return commandOutcome{}, errors.New("save-buffer requires a running daemon")
	}
	fs := commandFlagSet("save-buffer")
	appendFile := fs.Bool("a", false, "append")
	name := fs.String("b", "", "buffer name")
	if err := fs.Parse(args); err != nil {
		return commandOutcome{}, err
	}
	if len(fs.Args()) != 1 {
		return commandOutcome{}, errors.New("save-buffer requires a path")
	}
	data, resolved, exists := d.pasteBuffers.get(*name)
	if !exists {
		return commandOutcome{}, fmt.Errorf("no paste buffer %q", resolved)
	}
	if fs.Arg(0) == "-" {
		return commandOutcome{Stdout: data}, nil
	}
	path, err := resolveCommandFilePath(fs.Arg(0), commandWorkingDirectory(ctx))
	if err != nil {
		return commandOutcome{}, err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if *appendFile {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(filepath.Clean(path), flags, 0o600)
	if err != nil {
		return commandOutcome{}, err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return commandOutcome{}, writeErr
	}
	return commandOutcome{}, closeErr
}

type pasteBufferOptions struct {
	name         string
	delete       bool
	bracketed    bool
	raw          bool
	unsanitized  bool
	separator    string
	separatorSet bool
}

func parsePasteBufferArgs(args []string) (pasteBufferOptions, error) {
	fs := commandFlagSet("paste-buffer")
	options := pasteBufferOptions{separator: "\r"}
	fs.StringVar(&options.name, "b", "", "buffer name")
	fs.BoolVar(&options.delete, "d", false, "delete after paste")
	fs.BoolVar(&options.bracketed, "p", false, "bracketed paste")
	fs.BoolVar(&options.raw, "r", false, "raw linefeeds")
	fs.BoolVar(&options.unsanitized, "S", false, "do not sanitize")
	fs.Func("s", "separator", func(value string) error {
		options.separator = value
		options.separatorSet = true
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return pasteBufferOptions{}, err
	}
	if len(fs.Args()) != 0 {
		return pasteBufferOptions{}, errors.New("paste-buffer accepts no positional arguments")
	}
	if options.raw && options.separatorSet {
		return pasteBufferOptions{}, errors.New("paste-buffer cannot combine -r and -s")
	}
	return options, nil
}

func sanitizePasteBuffer(data []byte) []byte {
	var output bytes.Buffer
	for _, b := range data {
		switch b {
		case '\n', '\r', '\t':
			output.WriteByte(b)
		case 0x20, 0x21, 0x23:
			output.WriteByte(b)
		default:
			if b < 0x20 || b == 0x7f {
				output.WriteByte('^')
				if b == 0x7f {
					output.WriteByte('?')
				} else {
					output.WriteByte(b + '@')
				}
			} else {
				output.WriteByte(b)
			}
		}
	}
	return output.Bytes()
}

func preparePasteBufferData(data []byte, options pasteBufferOptions, bracketedPaste bool) []byte {
	if !options.unsanitized {
		data = sanitizePasteBuffer(data)
	}
	if !options.raw {
		data = bytes.ReplaceAll(data, []byte{'\n'}, []byte(options.separator))
	}
	if options.bracketed && bracketedPaste {
		data = append(append([]byte("\x1b[200~"), data...), []byte("\x1b[201~")...)
	}
	return data
}

func runPasteBufferCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	_, client, remaining, err := resolveSessionCommandContextValue(d, ctx, sessionTarget, args)
	if err != nil {
		return commandOutcome{}, err
	}
	if client == nil {
		return commandOutcome{}, errors.New("command requires an attached client")
	}
	return commandOutcome{Action: pasteClientBufferAction{
		ClientID:  client.ID,
		SessionID: client.SessionID,
		Args:      append([]string(nil), remaining...),
	}}, nil
}

func pasteBufferToClient(s *ClientInstance, args []string) error {
	options, err := parsePasteBufferArgs(args)
	if err != nil {
		return err
	}
	if s == nil || s.Daemon == nil {
		return errors.New("paste-buffer requires a running daemon")
	}
	data, resolved, exists := s.Daemon.pasteBuffers.get(options.name)
	if !exists {
		return fmt.Errorf("no paste buffer %q", resolved)
	}
	pane := s.activePane()
	if pane == nil {
		return errors.New("paste-buffer requires an active pane")
	}
	data = preparePasteBufferData(data, options, pane.InputMode().bracketedPaste)
	if err := pane.sendInput(data); err != nil {
		return fmt.Errorf("paste-buffer: %w", err)
	}
	if options.delete {
		if err := s.Daemon.pasteBuffers.delete(resolved); err != nil {
			return err
		}
	}
	return nil
}
