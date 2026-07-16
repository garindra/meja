# Using `meja`

`meja` gives you a terminal session that can live on this machine or on another
machine over SSH. You can leave the session, come back later, and—if you give it
a name—rebuild its windows and panes from an automatic snapshot.

The same `meja` executable contains the client and the per-user server. For
remote sessions, install `meja` on both machines; OpenSSH still handles host
aliases, authentication, agents, and host-key verification.

## The everyday flow

### Start a local session

For a quick local session, just run:

```bash
meja
```

That creates an unnamed session in your current directory. The server starts
automatically if it is not already running.

If the session matters, name it:

```bash
meja new -s work
```

A name is easier to remember than a numeric ID, and only named sessions are
automatically persisted for later restoration.

You can also name an existing session from inside Meja by pressing `Ctrl+b`,
then `$`. Automatic snapshots begin once the session has a name.

### Start a remote session

Give `meja` an SSH host, `user@host`, or an alias from your OpenSSH config:

```bash
meja prod
meja user@prod
```

Those commands create a new, unnamed session on the remote machine. For a
remote session you expect to keep, use the explicit form and give it a name:

```bash
meja new -s deploy prod
```

You can also choose its starting directory or initial command:

```bash
meja new -s deploy -c /srv/app prod
meja new -s logs prod -- journalctl -f
```

The command after `--` runs only in the first pane. New windows and splits use
the target user's shell.

### Leave, then come back

Inside Meja, press `Ctrl+b`, then `d` to detach. Detaching closes your client,
but the live session and its processes keep running on the server.

Attach again by name or numeric ID:

```bash
# Local
meja attach -t work

# Remote
meja attach -t deploy prod
```

`attach` has the shorter alias `a`, so this is equivalent:

```bash
meja a -t deploy prod
```

Use `attach` while the original session is still alive. If its server or final
pane has exited, use `restore` instead.

### What automatic persistence means

Meja has two ways of helping you come back:

1. A detached live session keeps its existing processes running. `attach`
   reconnects to that same session.
2. A named session is also snapshotted roughly every five seconds. `restore`
   uses that snapshot to build a new session after the original is gone.

The snapshot is a restart recipe, not a frozen process image. It records the
session's windows, pane layout, active window and pane, working directories,
shells, and detected commands. It does not preserve process memory, terminal
contents, or the exact in-memory state of an application.

Restore a local or remote named session with:

```bash
meja restore -t work
meja restore -t deploy prod
```

By default, Meja starts fresh shells and types each saved command at its prompt
without pressing Enter. You can review each command before running it. To leave
the prompts empty or run the commands immediately, use:

```bash
meja restore -t work --commands=skip
meja restore -t work --commands=run
```

Restoration is explicit: Meja does not automatically restore snapshots when a
server starts. If a live session with the same name still exists, attach to it
instead.

### See which sessions exist

Keep `ls` as the fallback when you forget a name or ID:

```bash
meja ls
meja ls prod
```

It shows each session's numeric ID, name, and whether a client is currently
attached. Unnamed sessions appear as `<unnamed>`. `ls` shows live sessions; it
does not list snapshots whose original sessions have ended.

## In-session keys

Meja uses `Ctrl+b` as its command prefix. Press `Ctrl+b`, release it, then press
the command key. Normal typing continues to go to the focused pane.

### Sessions and panes

| Keys | Behavior |
| --- | --- |
| `Ctrl+b`, `Ctrl+b` | Send a literal `Ctrl+b` to the focused pane. |
| `Ctrl+b`, `d` | Detach while leaving the session running. |
| `Ctrl+b`, `c` | Create a new window. |
| `Ctrl+b`, `%` | Split the focused pane left/right. |
| `Ctrl+b`, `"` | Split the focused pane top/bottom. |
| `Ctrl+b`, `↑` / `↓` / `←` / `→` | Focus the pane in that direction. |
| `Ctrl+b`, `Ctrl+↑` / `Ctrl+↓` / `Ctrl+←` / `Ctrl+→` | Move a pane boundary by one row or column. |
| `Ctrl+b`, `Alt+↑` / `Alt+↓` / `Alt+←` / `Alt+→` | Move a pane boundary by five rows or columns. |
| `Ctrl+b`, `z` | Toggle the focused pane between its split position and the full window. |
| `Ctrl+b`, `{` | Swap the focused pane with the previous pane. |
| `Ctrl+b`, `}` | Swap the focused pane with the next pane. |
| `Ctrl+b`, `x` | Close the focused pane. Closing the final pane ends the session. |

Each window supports up to eight visible panes.
Pane sizes are stored as part of the window layout and are included in named
session snapshots. After either resize binding, additional Ctrl- or
Alt-modified arrows repeat the resize without another prefix for 500
milliseconds after the previous adjustment.

Zoom keeps the underlying split layout intact. Hidden panes continue running,
and unzoom restores their previous sizes. Focusing another pane, resizing,
splitting, swapping, or closing the zoomed pane first leaves zoom mode.

### Windows and names

| Keys | Behavior |
| --- | --- |
| `Ctrl+b`, `n` | Select the next window. |
| `Ctrl+b`, `p` | Select the previous window. |
| `Ctrl+b`, `l` | Return to the last selected window. |
| `Ctrl+b`, `0`–`9` | Select the window with that status-bar index. |
| `Ctrl+b`, `,` | Rename the current window. |
| `Ctrl+b`, `$` | Name or rename the session, enabling snapshots once it has a name. |

Rename prompts appear in the status bar. Type the new name, use Backspace or
Delete to remove characters, press Enter to save, or press Escape or `Ctrl+c`
to cancel. If Meja asks before overwriting an existing snapshot, press `y` to
confirm; `n`, Enter, Escape, or `Ctrl+c` cancels.

### History mode

Press `Ctrl+b`, then `[` to open a frozen history view for the focused pane.
While that view is open, these keys do not need the `Ctrl+b` prefix:

| Keys | Behavior |
| --- | --- |
| `↑` / `↓` | Move one row backward or forward. |
| Page Up / Page Down | Move 12 rows backward or forward. |
| `Ctrl+u` / `Ctrl+d` | Move 6 rows backward or forward. |
| `g` / `G` | Jump to the oldest or newest position in the captured history. |
| `q`, Escape, or `Ctrl+c` | Leave history mode and return to the live pane. |

---

## Complete reference

### Command synopsis

```text
meja version
meja [global-options]
meja [global-options] <host> [-- command args...]
meja [global-options] new [options] [host [-- command args...]]
meja [global-options] attach|a [options] -t <session-id-or-name> [host]
meja [global-options] restore [options] -t <session-name> [host]
meja [global-options] ls [options] [host]
meja [global-options] server run|stop
```

Use `meja help`, `meja -h`, or `meja --help` to print the built-in synopsis.

### Argument and option placement

- Global options must appear before the command or host.
- Command-specific and SSH options must appear before the host. Parsing stops
  at the host argument.
- A command for the initial pane must appear after the host and `--`.
- Omitting the host selects the local server. Supplying a host selects the
  server belonging to the SSH-authenticated user on that machine.

An unrecognized first word is treated as a host, making `meja prod` shorthand
for `meja new prod`. The words `new`, `attach`, `a`, `restore`, `ls`, `server`,
`version`, and `help` are reserved. Use the explicit form for a host alias with
one of those names:

```bash
meja new server
```

### Global server selection

```text
-L <profile>       Use a named server profile.
-S <socket-path>   Use an exact server socket path.
```

`-L` and `-S` are mutually exclusive. With neither option, Meja uses the
`default` profile.

A profile resolves to `~/.meja/<profile>/meja.sock`, so the default socket is
`~/.meja/default/meja.sock`. Profile names may contain letters, digits, `.`,
`_`, and `-`. An exact `-S` path must be absolute.

The selector is resolved on the machine hosting the session. For example,
`meja -L work ls prod` lists sessions from the `work` profile on `prod`.

Each profile or socket is an isolated server with its own sessions, session-ID
sequence, and snapshots.

### Remote connection options

The following options are accepted by `new`, `attach`, `restore`, and `ls`:

```text
-i <identity-file>      Pass an SSH identity file.
--port <port>           Pass an SSH port.
--remote-path <path>    Select the remote meja executable (default: meja).
```

These options matter only when a host is supplied. If `--port` is omitted,
Meja lets OpenSSH choose the port from its configuration and defaults. The
remote executable path is used exactly as supplied.

### `new`

```text
meja [global-options] new [new-options] [connection-options]
meja [global-options] new [new-options] [connection-options] <host>
     [-- command args...]
```

`new` creates and attaches to a session. Without a host it creates the session
locally. With a host it creates the session remotely through SSH.

```text
-s <session-name>    Give the session a unique name.
-c <directory>       Set the starting directory.
--cwd <directory>    Alias for -c.
```

The starting directory applies to the initial pane and to later windows and
splits. It is resolved on the target machine and must be absolute or begin with
`~/`. Quote a remote home-relative path so your local shell does not expand it:

```bash
meja new -c '~/projects/app' prod
```

When `-c` is omitted, a local session inherits the invoking process's current
directory. A remote session starts in the remote user's home directory.

The command after `<host> --` applies only to the initial pane:

```bash
meja new prod -- /usr/bin/bash -l
```

Creating a session starts the selected server automatically when its socket is
missing or stale.

### `attach` / `a`

```text
meja [global-options] attach [connection-options]
     -t <session-id-or-name> [host]
meja [global-options] a [connection-options]
     -t <session-id-or-name> [host]
```

`-t` is required and accepts either a positive numeric ID or a session name.
Without a host, Meja connects directly through the local control socket. With a
host, it obtains the connection information through SSH.

```bash
meja attach -t 12
meja attach -t work
meja attach -i ~/.ssh/prod_ed25519 --port 2222 -t work prod
```

Attaching to a session that is already attached replaces the existing client.
`attach` does not start a missing server.

### `restore`

```text
meja [global-options] restore [connection-options]
     -t <session-name> [--commands=prepare|skip|run] [host]
```

`-t` is required and must be a session name, not a numeric ID. `restore` reads
that name's snapshot, creates a new session with the same name and layout, and
attaches to it. It fails when a live session with that name already exists.

`--commands` controls how each saved pane command is handled:

```text
prepare   Type the command without a newline so it can be reviewed (default).
skip      Start the shell without typing the command.
run       Type the command followed by a newline so it runs immediately.
```

Named sessions are snapshotted approximately every five seconds. For a profile,
snapshots are stored at `~/.meja/<profile>/snapshots/<session-name>.json`. With
`-S /path/to/meja.sock`, they are stored under
`/path/to/snapshots/<session-name>.json`. Snapshot directories use mode `0700`
and snapshot files use mode `0600`.

The JSON snapshot is deliberately readable and contains the snapshot version,
session name, save time, active window, windows, pane layout, pane working
directories, shells, and commands.

Restoring starts the selected server automatically when necessary.

### `ls`

```text
meja [global-options] ls [connection-options] [host]
```

`ls` prints the active sessions for the selected local or remote server:

```text
Active Sessions
ID  NAME       STATUS
1   <unnamed>  detached
2   work       attached
```

Rows are ordered by numeric ID. `ls` does not start a missing server.

### `server run` and `server stop`

```text
meja [global-options] server run
meja [global-options] server stop
```

`server run` runs the selected local per-user server in the foreground.
Session-creating commands normally start the same server in the background, so
running it manually is optional.

`server stop` cleanly disconnects active clients, stops the server, and prints
its PID when available. It does not start a missing server.

These commands are local. To manage a server on another machine, run `meja
server ...` through SSH on that machine.

### `version` and help

```bash
meja version
meja help
meja -h
meja --help
```

`version` prints `meja <version>` and accepts no additional arguments. The help
forms print the command synopsis.

### Session names

Session names must be valid UTF-8, at most 128 bytes, and not entirely numeric.
They cannot contain control characters, `/`, or `\`. Names must be unique
within one server profile or socket.

### Server and socket behavior

- `new` and `restore` start the selected server if needed.
- `attach`, `ls`, and `server stop` require an already-running server.
- Socket directories created by Meja use mode `0700`; sockets use mode `0600`.
- Meja does not relax permissions on an existing socket parent directory. The
  parent must already belong to the current user and have mode `0700`.
- A session ends when its final pane exits. Detaching alone does not end it.

### Diagnostics

Render diagnostics are controlled by environment variables:

```bash
MEJA_DEBUG=1 meja
MEJA_DEBUG_RENDER=1 meja attach -t work
MEJA_DEBUG_LOG=/tmp/meja-render.log meja attach -t work
```

`MEJA_DEBUG=1` enables all available diagnostics. `MEJA_DEBUG_RENDER=1` enables
render diagnostics specifically. Diagnostics go to stderr unless
`MEJA_DEBUG_LOG` names a file; setting that path also enables render
diagnostics.

### Private control command

`__control-v1` is the machine-facing SSH bootstrap interface used internally by
the client:

```text
meja [global-options] __control-v1 start-session [session-name]
meja [global-options] __control-v1 connect-session <session-id-or-name>
meja [global-options] __control-v1 restore-session <session-name> <command-mode>
meja [global-options] __control-v1 list-sessions
```

Start, connect, and restore emit one `MEJA_BOOTSTRAP_V1` JSON record. Listing
emits one `MEJA_SESSION_LIST_V1` JSON record. This interface is versioned and is
not intended for direct interactive use.
