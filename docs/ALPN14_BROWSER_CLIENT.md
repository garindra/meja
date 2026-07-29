# Implementing a browser client for `meja-quic/14`

This document is the interoperability handoff for updating the existing
`seniman-meja` frontend in parallel with the Go terminal client. It describes
the current protocol contract, especially structured status and client-local
prompt editing.

The protocol is not a terminal-byte mirror. The server owns pane terminal state,
layout, prompt lifecycle, and command meaning. The browser owns presentation,
pane scanout caches, status placement, and the transient text being edited in an
active Meja prompt.

## What changes in `seniman-meja`

The existing implementation is in the sibling repository at
`../seniman-meja/src/index.js`. It is a Node/Seniman bridge:
`@matrixai/quic` terminates the Meja QUIC connection in Node, `TerminalModel`
holds frontend state, and Seniman publishes that state to the browser. The work
does not require raw QUIC support in a web browser.

The relevant current integration points are:

- protocol constants near `MEJA_ALPN`;
- `encodeFrame`, `encodeString`, and `PayloadReader`;
- `decodeClientStatus`;
- `TerminalModel.setStatus`, `metadata`, `publish`, and `sendInput`;
- `consumeControlStream`;
- the writer closures installed by `attachMeja`;
- `App`'s `handleKeyDown`, `installTextInput`, and `handlePaste`; and
- the existing `.window-tabs` / `.window-tabs-status` UI.

The minimum implementation delta is:

1. Add `MSG_FRONTEND_PROMPT_RESULT = 14`.
2. Change `decodeClientStatus` from prompt
   `{ mode, label, text, cursor }` to
   `{ promptId, mode, label, initial }`, in that wire order.
3. Add prompt-result encoding and a writer installed by `attachMeja`.
4. Give `TerminalModel` a separate client-local prompt draft. Do not put draft
   text or cursor back into the decoded server status object.
5. In `TerminalModel.setStatus`, create the draft only for a new prompt ID,
   preserve it for the same ID, and clear it for a non-prompt status.
6. Route `sendInput` to the local editor while that draft is active. Its
   ordinary-input writer must not run in that branch.
7. Publish local prompt edits even though the server status revision has not
   changed. The current `App` subscription only updates `clientStatus` when its
   revision increases, so expose a separate locally revised `promptDraft` in
   `metadata()` and a corresponding Seniman state value.
8. Render the prompt label, draft, and caret in the existing bottom chrome. The
   hidden `#terminal-keyboard` textarea can continue to own DOM focus and IME
   input.
9. On submit/cancel, send one `FRONTEND_PROMPT_RESULT`, mark the draft resolved,
   and wait for the next server status.
10. Run `npm run build` so `dist/index.js` matches `src/index.js`; do not edit
    the generated file by hand.

`requestAttachGrant` currently reports `terminalRows: options.rows + 1` to the
local command socket, while the QUIC `SESSION_ATTACH` correctly sends
`options.rows` as the drawable browser grid. Keep that distinction: the fixed
24-pixel tab/status bar is browser chrome and is already subtracted by
`measureTerminal` before `sendResize`.

The current `seniman-meja` connection path creates a fresh attach grant and does
not implement `CLIENT_RESUME`. The reconnect rules later in this document are
the target contract if resume is added; prompt migration does not require adding
resume in the same change.

## Connection topology

Negotiate TLS ALPN `meja-quic/14`.

Each connection has:

- one client-initiated bidirectional control stream;
- exactly eight server-initiated unidirectional display streams; and
- no dedicated status stream.

Control frames use:

```text
uvarint message_type
uvarint payload_length
payload bytes
```

Integers inside payloads are unsigned varints, booleans are one byte (`0` or
`1`), and strings are a length varint followed by UTF-8 bytes. The default
maximum control-frame payload is 4 MiB; an individual protocol string is at
most 64 KiB.

The eight display streams have QUIC stream IDs `3, 7, 11, ... 31`. Their slot is
`(stream_id - 3) / 4`, producing slots 0 through 7. Each stream begins with a
display `NOOP` used to materialize it. Display streams are opcode streams, not
control-frame streams.

## Control message numbers

The current message mapping is:

| ID | Name | Direction |
|---:|---|---|
| 1 | `FRONTEND_INPUT_BYTES` | client to server |
| 2 | `FRONTEND_RESIZE` | client to server |
| 3 | `CLIENT_LAYOUT` | server to client |
| 4 | `SESSION_ATTACH` | client to server |
| 5 | `SESSION_ATTACH_OK` | server to client |
| 6 | `SESSION_ATTACH_FAILED` | server to client |
| 7 | `CLIENT_RESUME` | client to server |
| 8 | `CLIENT_RESUME_OK` | server to client |
| 9 | `FRONTEND_TERMINAL_WRITE` | server to client |
| 10 | `FRONTEND_REGISTER_TERMINAL_EXIT_COMMAND` | server to client |
| 11 | `FRONTEND_EXECUTE_TERMINAL_EXIT_COMMAND` | server to client |
| 12 | `FRONTEND_TERMINAL_EXIT_COMPLETE` | client to server |
| 13 | `CLIENT_STATUS` | server to client |
| 14 | `FRONTEND_PROMPT_RESULT` | client to server |

`seniman-meja` may treat the terminal setup/exit messages as no-ops because its
DOM keyboard capture is not a physical terminal. It must still consume them so
control framing remains synchronized. If it supports a server execute-exit
request, it acknowledges that request with
`FRONTEND_TERMINAL_EXIT_COMPLETE`.

## Attach and resume

Open the bidirectional control stream first. Its first client frame is exactly
one of:

```text
SESSION_ATTACH {
    token: string
    cols: uvarint
    rows: uvarint
}

CLIENT_RESUME {
    resume_token: string
    cols: uvarint
    rows: uvarint
}
```

`cols` and `rows` are the drawable pane cell grid. A browser should compute
these after laying out its own tabs, toolbar, and status UI. It must not subtract
the native client's one-row status bar unless its own design actually consumes
one grid row.

The successful reply is `SESSION_ATTACH_OK { resume_token }` or an empty
`CLIENT_RESUME_OK`. A new attach can instead receive
`SESSION_ATTACH_FAILED { reason }`.

After success, consume or deliberately ignore the registered terminal-exit
command and terminal-setup write, then accept all eight unidirectional display
streams. The current `consumeControlStream` already safely skips their unknown
frame types after consuming complete frames. The server initializes the view
only after those streams exist. `CLIENT_LAYOUT`, `CLIENT_STATUS`, and pane
render commands can subsequently arrive independently, so feed all of them into
one serialized model/UI state machine.

On a viewport change, send:

```text
FRONTEND_RESIZE {
    cols: uvarint
    rows: uvarint
}
```

Both values must be in `1..1024`.

## Layout and display activation

`CLIENT_LAYOUT` is a complete visible layout:

```text
{
    window_id: uvarint
    focused_pane_id: uvarint
    layout_revision: uvarint
    panes: [{
        pane_id: uvarint
        slot: uvarint
        rect: { x, y, width, height: uvarint }
    }]
}
```

At most eight panes are visible and each slot is in `0..7`. A display stream is
a reusable transport slot, not a permanent pane identity.

The first render command after a slot is bound is:

```text
START_RENDER {
    layout_revision: uvarint
    cols: uvarint
    rows: uvarint
}
```

It resets that display stream's compiler state. Buffer display operations until
`PRESENT`. Activate a `CLIENT_LAYOUT` revision only when the initial presented
frame for every visible pane in that revision is available. Validate that each
`START_RENDER` grid matches its pane rectangle. Frames for another revision
must not be painted into the current geometry.

Keep the last authoritative cell grid per slot. The display protocol can update
only changed regions, including full-width vertical `SCROLL_REGION` operations,
so a frame is applied to the cached grid and then painted. Local typing
prediction is only a presentation overlay and must never modify that
authoritative cache.

## Structured status

There is no ninth unidirectional status stream. `CLIENT_STATUS` is a complete
snapshot on the bidirectional control stream:

```text
{
    revision: uvarint
    session_id: uvarint
    session_name: string
    server_hostname: string
    server_home: string
    root: string
    windows: [{
        window_id: uvarint
        index: uvarint
        title: string
        active: bool
        zoomed: bool
    }]
    kind: byte                 // 1 normal, 2 prompt, 3 message
    prompt: {
        prompt_id: uvarint
        mode: byte             // 1 text, 2 confirm
        label: string
        initial: string
    }
    message: {
        id: uvarint
        text: string
    }
}
```

All fields are encoded for every kind. Fields outside the selected kind should
be ignored. A prompt status requires a nonzero `prompt_id`; status revisions
are also nonzero.

Keep the greatest accepted status revision and ignore any snapshot whose
revision is not greater. The snapshot is semantic data, not preformatted
terminal text. The browser chooses status placement, style, truncation, links,
and accessible markup.

## Prompt ownership and flow

The server sends only a prompt descriptor:

```text
CLIENT_STATUS(kind = prompt) {
    prompt: {
        prompt_id,
        mode,
        label,
        initial
    }
}
```

For a prompt ID the frontend owns:

- draft text;
- cursor or selection;
- decoding of keyboard, composition, and paste input;
- immediate rendering; and
- the decision that a local gesture means submit or cancel.

The server does not receive or retain transient draft edits. In
`seniman-meja`, this state belongs in `TerminalModel`, not in the server-status
decoder and not solely in a short-lived DOM handler.

When a new prompt ID arrives, create a draft from `initial` and place the cursor
at its end. If a newer status snapshot contains the same prompt ID, update the
descriptor but preserve the local draft and cursor. Reinitializing on every
status revision would erase typed-but-unsubmitted text.

While a prompt is active:

- route keyboard, IME, and paste events to the local prompt editor;
- do not send `FRONTEND_INPUT_BYTES`;
- render each edit immediately; and
- send exactly one structured result on submit or cancel.

The existing `App` input handlers already normalize desktop keydown, mobile
`beforeinput`/`input`, helper keys, and paste into strings before calling
`TerminalModel.sendInput`. The smallest compatible change is therefore a
bounded incremental local decoder in `TerminalModel`, mirroring the Go
client's `internal/client/prompt.go`. A larger refactor may pass semantic DOM
edit events instead, but all input sources must converge on the same prompt
draft and none may fall through to the ordinary QUIC input writer.

The result payload is:

```text
FRONTEND_PROMPT_RESULT {
    prompt_id: uvarint
    submitted: bool
    text: string
}
```

`prompt_id` must be nonzero and `text` must be valid UTF-8 of at most 64 KiB.
Use an empty string on cancel.

Suggested browser bindings for a text prompt are ordinary text/IME insertion,
Backspace, Delete, ArrowLeft, ArrowRight, Home, End, Enter to submit, and Escape
or Ctrl-C to cancel. A paste should be inserted as text; normalize line breaks
and tabs if the UI is single-line. Enforce the UTF-8 byte limit locally.

For a confirmation prompt:

- `y` or `Y` submits with text `"y"`;
- `n` or `N` cancels;
- Enter, Escape, and Ctrl-C cancel; and
- unrelated keys do nothing.

After sending a result, mark the local prompt resolved and suppress further
input until a newer `CLIENT_STATUS` says normal, message, or opens another
prompt. Do not send the same result again. Render normal window chrome while
waiting, or otherwise visually mark the prompt inactive. The server compares
the opaque prompt ID with its current lifecycle state and ignores a stale or
duplicate result.

The resulting sequence is:

```text
server                                   browser
  |                                        |
  | CLIENT_STATUS(prompt descriptor)       |
  |--------------------------------------->|
  |                                        | create local draft
  |                                        | edit and render locally
  |                                        | (no input frames)
  | FRONTEND_PROMPT_RESULT                 |
  |<---------------------------------------|
  | run the server-owned continuation      |
  | CLIENT_STATUS(normal/message/prompt)    |
  |--------------------------------------->|
```

The server still owns whether a prompt exists, which continuation it resolves,
and what submitted text means. Client-local editing does not make the frontend
authoritative for commands or pane terminal state.

## Ordinary input

Outside an active prompt, send terminal input as:

```text
FRONTEND_INPUT_BYTES {
    layout_revision: uvarint
    source_idle: bool
    data: remaining payload bytes
}
```

The layout revision is the geometry visible when the event was generated. The
server uses it for layout-sensitive routing such as pointer hit testing.
`source_idle` is only valid for a single deferred Escape byte; it tells the
server the local ambiguity window expired.

The server parses this input and re-encodes it for the focused pane's
authoritative terminal modes. A browser should therefore report frontend input
events in the encodings negotiated by the server setup rather than trying to
encode directly for the pane application.

As a defensive lifecycle rule, the server drops ordinary input received while
a prompt is active. This prevents a buggy or lagging frontend from injecting
prompt keystrokes into a pane, but it is not the normal prompt input path.

## Reconnect behavior

Retain the resume token in memory and use `CLIENT_RESUME` for a replacement
transport. Do not replay ordinary input or prompt results after a disconnect.
The replacement connection receives complete layout and status state.

Status revisions are allocated by the stable client identity and remain
monotonic across replacement transports. Continue rejecting stale revisions.
The active prompt continuation is transport-local, so the first complete status
on a replacement connection determines whether a prompt is active. A local
draft may remain visible while reconnecting, but discard it if the replacement
snapshot is not the same prompt ID; never invent or replay a result. This is a
future concern for `seniman-meja`, whose current `connect` path constructs a new
`TerminalModel` and obtains a new attach grant rather than resuming.

Render layout revisions are a separate coordinate-space mechanism. Do not
compare them with status revisions.

## Minimal browser state machine

```text
onStatus(status):
    if status.revision <= acceptedStatusRevision:
        return
    acceptedStatusRevision = status.revision

    if status.kind == PROMPT:
        if prompt == null or prompt.id != status.prompt.prompt_id:
            prompt = newDraft(status.prompt.initial, cursorAtEnd=true)
        prompt.descriptor = status.prompt
    else:
        prompt = null

    renderStatus()

onUserInput(event):
    if prompt != null:
        if prompt.resolved:
            return
        outcome = editPromptLocally(prompt, event)
        renderStatus()
        if outcome is submit or cancel:
            prompt.resolved = true
            send(FRONTEND_PROMPT_RESULT(outcome))
        return

    send(FRONTEND_INPUT_BYTES(currentLayoutRevision, event))
```

## Implementation checklist

- Negotiate exactly `meja-quic/14`.
- Use one bidirectional framed control stream.
- Accept exactly eight server unidirectional display streams and map their
  stream IDs to slots.
- Serialize layout frames, presented pane frames, status snapshots, local
  prompt edits, reconnect changes, and resizes through one UI state owner.
- Treat `CLIENT_STATUS` as a complete, monotonically revised semantic snapshot.
- Render all status UI on the browser side.
- Initialize a prompt draft only when its opaque prompt ID changes.
- Keep prompt draft text and cursor entirely local.
- Never send per-keystroke prompt input.
- Send one `FRONTEND_PROMPT_RESULT` and wait for the next status snapshot.
- Keep the locally edited prompt in `TerminalModel` and publish it independently
  of `CLIENT_STATUS.revision`.
- Exercise desktop keydown, mobile `beforeinput`/`input`, helper keys, paste,
  IME completion, and the resolved-but-waiting state.
- Rebuild `seniman-meja/dist/index.js` with `npm run build`.
- Keep layout revision, status revision, pane ID, slot, and prompt ID as
  distinct namespaces.
- Bound frames, strings, grids, escape sequences, paste, and retained scanout
  memory before allocation.

For exact codec and display-opcode behavior, use `internal/protocol` as the
normative implementation.
