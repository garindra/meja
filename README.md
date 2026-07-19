# meja

**tmux-style workspaces that survive bad networks and rebuild from a file.**

<p align="center">
  <img src="screenshot.png" alt="Meja screenshot" width="800">
</p>

Meja brings tmux’s familiar workflow to local and remote sessions, keeping remote work responsive through latency and alive through disconnects. 

Sessions can be recovered after reboots, reconstructing the same windows, panes, directories, and commands. Readable `.meja` files can live in version control, letting anyone recreate them on their machines.

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
meja attach -t work -h devbox
```

If the machine reboots—or the session otherwise ends—recover it from its latest recorded state:

```sh
meja restore -t work -h devbox
```

Meja creates and enters a new session with the saved windows, panes, directories, and prepared commands.


You can also run `save` to save the current session as a readable and editable `.meja` file:

```sh
meja save -t work -o dev.meja
```

Then create a fresh session from it:

```
meja new -f dev.meja
```

The file is readable and editable. Check it into version control so your team can recreate the same session on their machines.

---

## Installation

On Linux x86-64:

```sh
curl -fsSL https://github.com/garindra/meja/releases/download/v0.0.2/meja_0.0.2_linux_amd64.tar.gz \
  | sudo tar -xz -C /usr/local/bin meja
```

On macOS with Apple Silicon:

```sh
curl -fsSL https://github.com/garindra/meja/releases/download/v0.0.2/meja_0.0.2_darwin_arm64.tar.gz \
  | sudo tar -xz -C /usr/local/bin meja
```

You can also install the latest version with Go:

```sh
go install github.com/garindra/meja@latest
```

Check that your meja installation is successful:

```sh
meja version
```

For remote `meja -h <host>` sessions, make sure to also install the binary on the target machine, reachable on `$PATH`.

#### Build from source:

```sh
git clone https://github.com/garindra/meja
cd meja
go build -o bin/meja .
```


---

## `.meja` files

A `.meja` file describes how to recreate a development session: its windows, panes, directories, and commands.

For example, a typical project might have a development window for the app and another for its services:

```kdl
root "."

window name="dev" {
    layout "main-vertical"

    pane {
        cwd "frontend/"
        cmd "npm run dev"
    }

    pane {
        cwd "backend/"
        cmd "go run ."
    }
}

window name="services" {
    layout "even-horizontal"

    pane {
        cmd "mysql"
    }

    pane {
        cmd "redis-server"
    }
}
```

Create a new session from the file:

```sh
meja new -f dev.meja
```

The file is readable and editable. Check it into version control so your team can recreate the same session on their machines.

---

## Canonical project flow

A typical team workflow:

Start a session on your development machine:

```sh
meja -h devbox
```

Move into your project:

```sh
cd ~/projects/acme
```

Set the session root:

```sh
meja set-root .
```

Arrange your windows, panes, directories, and running processes.

Save the session:

```sh
meja save -t work -o dev.meja
```

Commit the file with the project:

```sh
git add dev.meja
git commit
```

Now anyone on the team can recreate the same development environment:

```sh
meja new -f dev.meja
```
---

## Why `meja`

### Familiar by design

Meja follows tmux wherever practical.

It uses the same `Ctrl+b` prefix and the familiar model of sessions, windows, panes, splits, history, detaching, and attaching. Existing tmux knowledge transfers directly.

### Editable `.meja` files

`meja save` writes a live session's plan to a readable `.meja` file.

These files can be edited, kept with a project, checked into version control, and used by other people to create the same session arrangement.


### Named-session recovery

Meja persists the recoverable state of every named session.

After a reboot or ended session, `meja restore -t <session-name>` creates a new session from its latest recovery record. It preserves the session's windows, panes, layout, working directories, shells, commands, and active selection—not process memory or application state.

### The client survives disconnections

A dropped connection does not close the client or return you to the local shell.

Meja keeps the last confirmed terminal contents visible, displays its connection status, and reconnects automatically. Once connected again, it applies the latest layout and complete visible pane contents before normal input resumes.

### Latency compensation

Eligible keystrokes are applied optimistically on the client, keeping remote typing responsive while waiting for the server's render.

The server remains authoritative. Confirmed renders reconcile the local display with the actual terminal state.
