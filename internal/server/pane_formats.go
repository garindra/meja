package server

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

func listPanesCommand() commandHandler {
	return func(ctx *commandContext, args []string) (commandExecution, error) {
		session, _, normalized, err := resolveSessionCommandContext(ctx, sessionTarget, args)
		if err != nil {
			return commandExecution{}, err
		}
		fs := commandFlagSet("list-panes")
		format := fs.String("F", "#{pane_id}: #{pane_in_mode}", "format")
		if err := fs.Parse(normalized); err != nil {
			return commandExecution{}, err
		}
		if len(fs.Args()) != 0 {
			return commandExecution{}, errors.New("list-panes accepts no positional arguments")
		}
		var output strings.Builder
		err = session.coordinate(func() error {
			ids := make([]uint64, 0, len(session.Panes))
			for id := range session.Panes {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			for _, id := range ids {
				pane := session.Panes[id]
				if pane == nil {
					continue
				}
				output.WriteString(expandPaneFormat(*format, session, pane))
				output.WriteByte('\n')
			}
			return nil
		})
		if err != nil {
			return commandExecution{}, err
		}
		return commandExecution{result: commandResult{stdout: []byte(output.String())}}, nil
	}
}

func expandPaneFormat(format string, session *Session, pane *Pane) string {
	if session == nil || pane == nil {
		return format
	}
	inMode := "0"
	if pane.isHistoryMode() {
		inMode = "1"
	}
	cols, rows := pane.TerminalSize()
	values := map[string]string{
		"pane_id":           strconv.FormatUint(pane.ID, 10),
		"pane_index":        strconv.FormatUint(pane.ID, 10),
		"pane_in_mode":      inMode,
		"session_id":        strconv.FormatUint(session.ID, 10),
		"session_name":      session.SessionName(),
		"pane_width":        strconv.Itoa(cols),
		"pane_height":       strconv.Itoa(rows),
		"pane_in_copy_mode": inMode,
	}
	var output strings.Builder
	for len(format) > 0 {
		start := strings.Index(format, "#{")
		if start < 0 {
			output.WriteString(format)
			break
		}
		output.WriteString(format[:start])
		end := strings.IndexByte(format[start+2:], '}')
		if end < 0 {
			output.WriteString(format[start:])
			break
		}
		end += start + 2
		name := format[start+2 : end]
		if value, ok := values[name]; ok {
			output.WriteString(value)
		} else {
			output.WriteString(format[start : end+1])
		}
		format = format[end+1:]
	}
	return output.String()
}
