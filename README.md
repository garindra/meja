# meja

```{=html}
<p align="center">
```
`<strong>`{=html}tmux-style workspaces that survive bad networks and
rebuild from a file.`</strong>`{=html}
```{=html}
</p>
```
```{=html}
<p align="center">
```
`<img src="screenshot.png" alt="Meja screenshot" width="800">`{=html}
```{=html}
</p>
```
```{=html}
<p align="center">
```
Local and remote terminal sessions with persistent layouts, reconnecting
clients, and reproducible environments.
```{=html}
</p>
```

------------------------------------------------------------------------

## ✨ Overview

Meja brings tmux's familiar workflow to local and remote sessions,
keeping remote work responsive through latency and alive through
disconnects.

Sessions can be recovered after reboots, reconstructing the same
windows, panes, directories, and commands.

Readable `.meja` files can live in version control, letting anyone
recreate the same workspace on their machine.

------------------------------------------------------------------------

## 🚀 One workflow, local or remote

### Start a local session

``` sh
meja
```

### Connect to another machine

``` sh
meja -h user@host
```

SSH aliases are supported:

``` sh
meja -h devbox
```

If the network disappears, the client stays open. Meja preserves the
last confirmed screen, shows reconnecting status, and continues
automatically when the connection returns.

### Session lifecycle

Rename:

``` sh
meja rename work
```

Detach:

``` sh
meja detach
```

Attach later:

``` sh
meja attach -t work -h devbox
```

Restore:

``` sh
meja restore -t work -h devbox
```

------------------------------------------------------------------------

## 💾 Save and share environments

Save a session:

``` sh
meja save -t work -o dev.meja
```

Create from a file:

``` sh
meja new -f dev.meja
```

`.meja` files are readable and editable, making them suitable for
version control and team workflows.

------------------------------------------------------------------------

## 📦 Installation

### Linux x86-64

``` sh
curl -fsSL https://github.com/garindra/meja/releases/download/v0.0.4/meja_0.0.4_linux_amd64.tar.gz \
  | sudo tar -xz -C /usr/local/bin meja
```

### macOS Apple Silicon

``` sh
curl -fsSL https://github.com/garindra/meja/releases/download/v0.0.4/meja_0.0.4_darwin_arm64.tar.gz \
  | sudo tar -xz -C /usr/local/bin meja
```

### Install with Go

``` sh
go install github.com/garindra/meja@latest
```

Verify:

``` sh
meja version
```

For remote sessions, install the binary on the target machine and ensure
it is available on `$PATH`.

### Build from source

``` sh
git clone https://github.com/garindra/meja
cd meja
go build -o bin/meja .
```

------------------------------------------------------------------------

## 📄 `.meja` files

A `.meja` file describes how to recreate a development session:

-   windows
-   panes
-   layouts
-   directories
-   commands

Example:

``` kdl
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
```

Create a session:

``` sh
meja new -f dev.meja
```

------------------------------------------------------------------------

## 🔄 Canonical project flow

``` sh
meja -h devbox

cd ~/projects/acme

meja set-root .

meja save -t work -o dev.meja

git add dev.meja
git commit
```

Anyone can recreate the environment:

``` sh
meja new -f dev.meja
```

------------------------------------------------------------------------

# Why `meja`

## Familiar by design

Meja follows tmux wherever practical.

It uses the same `Ctrl+b` prefix and familiar concepts:

-   sessions
-   windows
-   panes
-   splits
-   history
-   detaching
-   attaching

## Editable `.meja` files

`meja save` writes a live session's plan to a readable `.meja` file.

These files can be edited, stored with projects, checked into version
control, and shared with teammates.

## Named-session recovery

Meja persists recoverable state for every named session.

After a reboot or ended session:

``` sh
meja restore -t <session-name>
```

creates a new session from the latest recovery record.

It preserves windows, panes, layout, working directories, shells,
commands, and active selection --- but not process memory or application
state.

## The client survives disconnections

A dropped connection does not close the client or return you to your
local shell.

Meja keeps the last confirmed terminal contents visible, displays
connection status, reconnects automatically, and reapplies the latest
layout.

## Latency compensation

Eligible keystrokes are applied optimistically on the client.

The server remains authoritative, and confirmed renders reconcile the
local display with the actual terminal state.