<h1 align="center">
  <img src="logo.png" alt="Meja logo" width="180">
  <br>
  MEJA
</h1>

<div align="center">
  <img src="screenshot.png" alt="Meja screenshot" width="840">
  <p><em>"what is this, a blue tmux?"</em></p>
</div>

## Overview

`meja` is a tmux-style multiplexer with native remote capability and restorable &amp; shareable sessions. It brings tmux's familiar workflow to local and remote sessions, keeping remote work responsive through latency and alive through disconnects.

Sessions can be recovered after reboots, reconstructing the same windows, panes, working directories, and commands.
Readable `.meja` files can live in version control, letting anyone recreate the same workspace on their machine.

## Installation

On Linux x86-64:

```sh
curl -fsSL https://github.com/garindra/meja/releases/download/v0.0.11/meja_0.0.11_linux_amd64.tar.gz \
  | sudo tar -xz -C /usr/local/bin meja
```

On macOS with Apple Silicon:

```sh
curl -fsSL https://github.com/garindra/meja/releases/download/v0.0.11/meja_0.0.11_darwin_arm64.tar.gz \
  | sudo tar -xz -C /usr/local/bin meja
```

You can also install the latest version with Go:

```sh
go install -v github.com/garindra/meja@latest
```

Check that your meja installation is successful:

```sh
meja version
```

> [!NOTE]
> For remote `meja -h <host>` sessions, make sure to also install the binary on the target machine, reachable on `$PATH`.


---

## One familiar workflow, local or remote

Start a new session on your machine by running:

```sh
meja
```

This starts a session with tmux-style windows and panes you know and love, and keybindings you're familiar with.

You can also start one on another machine:

```sh
meja -h user@host
```

`-h` also accepts aliases from your system’s SSH configuration:

```sh
meja -h devbox
```

If the network disappears, the client stays open. Meja preserves the last confirmed screen, shows that it is reconnecting, and continues when the connection returns.

Rename the session from inside it:

```sh
meja rename work
```

Detach from it:

```sh
meja detach
```

Meja keeps the session and its processes running. Return to it later:

```sh
meja -h devbox attach -t work
```

If the machine reboots—or the session otherwise ends—recover it from its latest recorded state:

```sh
meja -h devbox restore -t work
```

Meja creates and enters a new session with the saved windows, panes, directories, and prepared commands.


You can also run `save` to save the current session as a readable and editable `.meja` file:

```sh
meja save -t work -o dev.meja
```

Then create a fresh session from it and again have your workspace reconstructed:

```
meja new -f dev.meja
```

The file is readable and editable. Check it into version control so your team can recreate the same session on their machines.

---

## Why `meja`

### Session recovery

Meja persists the recoverable state of every named session.

After a reboot or ended session, `meja restore -t <session-name>` creates a new session from its latest recovery record. It preserves the session's windows, panes, layout, working directories, shells, commands, and active selection.

### Shareable `.meja` files

`meja save` writes a live session's reconstructable workspace state to a
readable `.meja` file.

These files can be edited, kept with a project, checked into version control, and used by other people to create the same session arrangement.

See the [`.meja` file format](REFERENCE.md#project-meja-files).

### The client survives disconnections

A dropped connection does not close the client or return you to the local shell.

Meja keeps the last confirmed terminal contents visible, displays its connection status, and reconnects automatically. Once connected again, it applies the latest layout and complete visible pane contents before normal input resumes.

### Latency compensation

Eligible keystrokes are applied optimistically on the client, keeping remote typing responsive while waiting for the server's render.

The server remains authoritative. Confirmed renders reconcile the local display with the actual terminal state.

### Familiar by design

Meja uses tmux's default `Ctrl+b` prefix and familiar model of sessions,
windows, panes, splits, history, detaching, and attaching. Existing tmux users
should feel at home.

> Meja follows tmux wherever practical. See [REFERENCE.md](REFERENCE.md) for
> currently supported commands and functionality.

## Everyday keybindings

Like tmux, Meja uses `Ctrl+b` as its command prefix. Press `Ctrl+b`, release
it, then press the command key. Normal typing continues to the focused pane.

| Keys | Everyday use |
| --- | --- |
| `Ctrl+b`, `d` | Detach while leaving the session running. |
| `Ctrl+b`, `c` | Create a window. |
| `Ctrl+b`, `%` / `"` | Split left/right or top/bottom. |
| `Ctrl+b`, arrow | Focus a pane in that direction. |
| `Ctrl+b`, `Ctrl+arrow` / `Alt+arrow` | Resize by one or five cells. |
| `Ctrl+b`, `n` / `p` / `l` | Select the next, previous, or last window. |
| `Ctrl+b`, `0`–`9` | Select a window by status-bar index. |
| `Ctrl+b`, `Space` | Cycle through preset pane layouts. |
| `Ctrl+b`, `z` | Toggle pane zoom. |
| `Ctrl+b`, `[` / `]` | Enter copy mode or paste the newest buffer. |
| `Ctrl+b`, `x` | Confirm and close the focused pane. |
| `Ctrl+b`, `:` | Open the command prompt. |

See [all keybindings and in-session interactions](REFERENCE.md#in-session-interaction).

---

## Documentation

See [REFERENCE.md](REFERENCE.md) for the complete command, option, keybinding, profile, socket, autosave, and diagnostics reference.

See [ARCHITECTURE.md](ARCHITECTURE.md) for a detailed overview of Meja's internals, including the SSH bootstrap flow, QUIC transport layer, reconnection model, latency compensation strategy, rendering pipeline, session lifecycle, persistence format, protocol design, and security considerations.
