# Meja Reference

This is the complete user-facing command, file-format, and interaction reference for Meja. See [README.md](README.md) for the introduction and installation instructions.

## Contents

- [Invocation and transport](#invocation-and-transport)
- [Session lifecycle commands](#session-lifecycle-commands)
- [Attached-session commands](#attached-session-commands)
- [Targets and in-pane context](#targets-and-in-pane-context)
- [Named-session recovery](#named-session-recovery)
- [Project `.meja` files](#project-meja-files)
- [In-session interaction](#in-session-interaction)
- [Server and remote behavior](#server-and-remote-behavior)
- [Pane environment and diagnostics](#pane-environment-and-diagnostics)
- [Private SSH command](#private-ssh-command)

---

# Invocation and transport

## Synopsis

```text
meja version
meja [transport-options] [command [command-args...]]
```

With no command, Meja runs `new-session` (`new`).

Transport options must precede the command name. The client removes the leading transport options before forwarding the command and all remaining arguments to the selected server:

```sh
meja -h prod new -s work
meja -h prod new -s work        # transport options must stay before `new`
meja -h prod new -- journalctl -f
```

Everything at and after `--` is preserved as command input.

## Server selection

```text
-L <profile>       Select a named server profile.
-S <socket-path>   Select an exact server socket.
```

`-L` and `-S` are mutually exclusive. The default profile resolves to:

```text
~/.meja/default/meja.sock
```

Other profiles use `~/.meja/<profile>/meja.sock`. Profile names may contain letters, digits, `.`, `_`, and `-`. Exact socket paths must be absolute. For remote commands, profiles and socket paths are resolved on the remote machine.

Each profile or exact socket has an isolated server, session-ID sequence, live sessions, client identities with resume tokens, and recovery files.

## Remote options

```text
-h, --host <host>       Hostname, user@host, or OpenSSH alias.
-i <identity-file>      SSH identity file.
--port <port>           SSH port.
--remote-path <path>    Remote meja executable (default: meja).
```

Meja invokes the system `ssh` executable, so OpenSSH handles aliases, authentication, agents, host keys, jump hosts, and proxy commands. The remote executable must already exist; Meja does not install or upload it.

## Help

```sh
meja help
meja --help
meja help resize-pane
meja resize-pane --help
meja -h prod help
```

Help is server-backed and may start the selected server. `version` is a local client command and prints `meja <version>`.

## Public names and aliases

| Command | Aliases |
| --- | --- |
| `new-session` | `new` |
| `attach-session` | `attach`, `a` |
| `restore-session` | `restore` |
| `save-session` | `save` |
| `list-sessions` | `ls` |
| `new-window` | `neww` |
| `split-window` | `splitw` |
| `detach-client` | `detach` |
| `next-window` | `next` |
| `previous-window` | `prev` |
| `last-window` | `last` |
| `select-window` | `selectw` |
| `kill-pane` | `killp` |
| `swap-pane` | `swapp` |
| `select-pane` | `selectp` |
| `resize-pane` | `resizep` |
| `rename-window` | `renamew` |
| `rename-session` | `rename`, `renames` |

---

# Session lifecycle commands

## `new-session` / `new`

```text
meja [transport-options] new-session
     [-d] [-P] [-F format]
    [-s name]
    [-r directory | --root directory]
    [-- initial-command args...]

meja [transport-options] new-session
     -d -t base-session -s mirror-session

meja [transport-options] new-session
     -f file.meja
     [-s name]
     [--commands=prepare|skip|run]
```

Examples:

```sh
meja
meja new -s work
meja -h prod new -s deploy -r /srv/app
meja -h prod new -s logs -- journalctl -f
meja new -f dev.meja -s dev-alice
```

`-s` assigns a unique name and enables private recovery persistence. `-r`/`--root` accepts an absolute path, `~`, `~/...`, or a path relative to the invoking directory on the selected machine. The resolved directory must exist.

Without `-r`, a local session uses the caller's directory. A remote session normally starts in the remote forwarding process's directory, usually the remote user's home.

The command after `--` runs directly as the first pane process. Later windows and splits start login shells.

`-f` creates a fresh session from a project file. It cannot be combined with a root or initial command. Relative file paths are resolved on the selected machine. The filename without its extension is the default session name; `-s` overrides it.

Project-file commands use:

```text
prepare   Type without Enter for review (default).
skip      Leave the new shell prompt empty.
run       Type and press Enter immediately.
```

`new` starts the selected server when needed.

`-d` creates the session and its initial pane without attaching, including
when stdin is not a terminal. `-P` prints one line after successful creation;
`-F` controls that line and defaults to `#{session_id}:#{pane_id}`. A normal
new session's pane ID is its initial pane ID. These flags are supported for
ordinary creation; they are rejected with `-f` project-file creation because
the restored file may contain multiple panes and does not have one implicit
initial pane for deterministic output.

`new-session -d -t base-session -s mirror-session` creates a detached grouped
session. The base target follows normal session target rules. The mirror must
have a unique name and cannot specify an initial command, root, or project
file. It receives links to the existing windows and panes; no PTY, process, or
pane is created. If the base is already grouped, the mirror joins that group.
Window lists and canonical layouts are shared, while session names, active and
previous windows, focus, zoom, prompts, and status state remain independent.

A window can be viewed by at most one attached client at a time. Selecting a
window currently viewed by another grouped session fails without changing the
requesting session's active window or focus. Detached sessions retain their
remembered view but hold no live view lease until attached.

If a viewed window disappears, Meja first tries the session's previous
window, then the lowest display-index window that is not leased by another
client. Explicit window kills are rejected when an attached grouped session
would have no such fallback. A pane exit uses the same order; if no fallback
exists, the affected client is closed cleanly rather than left bound to a
destroyed pane.

Process monitoring follows the daemon-global pane ID rather than the session
that created the pane. Removing one grouped session therefore leaves
monitoring and the shared process target attached to the surviving graph.

## `attach-session` / `attach` / `a`

```text
meja [transport-options] attach-session -t session-id-or-name
```

```sh
meja attach -t 12
meja -h prod a -t work
```

`-t` is required. Attach returns to the same live processes. A session has at most one attached client; a new attach replaces the previous client cleanly. Attach does not start a missing server.

## `restore-session` / `restore`

```text
meja [transport-options] restore-session
     -t session-name
     [-s new-name]
     [--commands=prepare|skip|run]
```

```sh
meja restore -t work
meja -h prod restore -t deploy
meja restore -t work -s recovered --commands=run
```

`-t` must be a name, not a numeric ID. Restore reads the selected server's private recovery file, creates a new live session and processes, and attaches. `-s` changes the new name. It fails if a live session already uses that name.

Restore does not read arbitrary project files; use `new -f`. It starts the selected server when needed, but restoration is always explicit.

## `save-session` / `save`

```text
meja [transport-options] save-session
     [-t session-id-or-name]
     -o file.meja
     [-f]
```

```sh
meja save -t work -o dev.meja
meja save -o dev.meja                 # inside a Meja pane
meja -h prod save -t work -o ~/projects/acme/dev.meja
```

Inside a Meja pane, omitting `-t` selects the session identified by the
injected `MEJA_SESSION_TARGET`. Outside Meja, `-t` remains required.

The live session does not need a name. User-owned `.meja` files do not embed
one; when the file is restored without `-s`, its filename supplies the default
session name (`dev7.meja` restores as `dev7`).

A relative output path is resolved from the captured session root. Successful
save output prints both that root and the absolute destination path. Remote
output is written remotely; Meja does not transfer it to the client.

If the command's current directory differs from the session root, save warns
and prints both paths. If the current directory is the intended project root,
run `meja set-root .` there and save again. Reconstructed pane paths will then
be relative to that root, making reconstruction more portable when the project
directory is mirrored on another machine.

Existing files are refused unless `-f` is supplied. Parent directories are created. Files are atomically replaced with mode `0644`.

Save normalizes inherited paths, preset layouts, custom geometry, commands, and non-default shells. Paths under the session root become relative where practical. Absolute pane paths outside the root are retained with a portability warning. Review captured pane commands and scrub any sensitive values before sharing or committing the file.

Save requires a running server and live session.

## `list-sessions` / `ls`

```text
meja [transport-options] list-sessions [-F format]
```

```text
Active Sessions
ID  NAME       STATUS
1   <unnamed>  detached
2   work       attached
```

Rows are ordered by ID. Only live sessions are listed. `ls` does not start a missing server.

With `-F`, the human-readable table is replaced by one expanded line per
session, ordered by numeric session ID.

## Server commands

```text
meja [-L profile | -S socket] start-server
meja [transport-options] kill-server
```

`start-server` runs a selected local server in the foreground. To start one remotely, invoke it through SSH yourself. `kill-server` may be forwarded with `-h`; it cleanly disconnects clients, stops the daemon, and reports its PID. It does not start a missing server.

---

# Attached-session commands

Prefix bindings and commands submitted by the `Ctrl+b, :` prompt use the same command engine. The prompt editor itself is attached-client UI, not a callable command. Most commands can also be run from an outside shell by supplying `-t`.

Commands launched from a Meja pane inherit its session target, so session-scoped
commands may omit `-t`. An explicit `-t` still overrides that default. Commands
whose target means a destination or creation input (`attach`, `restore`,
`switch-session`, and mirror creation with `new-session -t`) remain explicit.

| Command | Usage | Effect |
| --- | --- | --- |
| `new-window` | `new-window [-t session]` | Create a window at the session root. |
| `next-layout` | `next-layout [-t session]` | Cycle the active window's preset layout. |
| `split-window` | `split-window [-t session] [-h|-v]` | Split left/right (`-h`) or top/bottom (`-v`, also the default). |
| `detach-client` | `detach-client [-t session]` | Detach the client. |
| `next-window` | `next-window [-t session]` | Select the next window. |
| `previous-window` | `previous-window [-t session]` | Select the previous window. |
| `last-window` | `last-window [-t session]` | Return to the last window. |
| `select-window` | `select-window -t [session:]window` | Select a zero-based window index; the session may be omitted inside a Meja pane. |
| `kill-session` | `kill-session [-t session]` | Terminate a session and its panes. |
| `kill-pane` | `kill-pane [-t session]` | Close the active pane; attached execution asks for confirmation, while CLI execution is immediate. |
| `copy-mode` | `copy-mode [-t session]` | Enter history/copy mode. |
| `send-keys` | `send-keys [-t session] [-X copy-mode-command | -l] key...` | Send keys or run a copy-mode command in the active pane. |
| `capture-pane` | `capture-pane [-t session] [-p] [-b buffer-name] [-S start-line] [-E end-line] [-e] [-C] [-J] [-N]` | Capture the active pane into stdout or a paste buffer. |
| `list-panes` | `list-panes [-a] [-t session] [-F format]` | List panes and pane mode values. |
| `set-buffer` | `set-buffer [-a] [-b buffer-name] [-n new-buffer-name] data` | Create or update a paste buffer. |
| `show-buffer` | `show-buffer [-b buffer-name]` | Print a paste buffer. |
| `list-buffers` | `list-buffers` | List paste buffers. |
| `delete-buffer` | `delete-buffer [-b buffer-name]` | Delete a paste buffer. |
| `load-buffer` | `load-buffer [-b buffer-name] path` | Load a file into a paste buffer. |
| `save-buffer` | `save-buffer [-a] [-b buffer-name] path` | Save a paste buffer to a file. |
| `paste-buffer` | `paste-buffer [-t session] [-b buffer-name] [-dprS] [-s separator]` | Paste a buffer into the active pane. |
| `swap-pane` | `swap-pane [-t session] (-U|-D)` | Swap with the previous or next pane. |
| `select-pane` | `select-pane [-t session] (-U|-D|-L|-R)` | Focus an adjacent pane. |
| `resize-pane` | `resize-pane [-t session] ((-U|-D|-L|-R) [amount] | -Z)` | Resize by cells or toggle zoom. |
| `rename-window` | `rename-window [-t session:window] [name]` | Rename, or prompt when attached and omitted. |
| `rename-session` | `rename-session [-t session] [name]` | Name/rename, or prompt when attached and omitted. |
| `set-root` | `set-root [-t session] [directory]` | Change the session root. |
| `switch-session` | `switch-session -t session` | Move the current client to another live session. |

Direction flags are `-U`, `-D`, `-L`, and `-R`. Resize defaults to one cell; an optional amount must be positive. `swap-pane` accepts only `-U` and `-D` for previous/next pane order.

`set-root` without a path uses the active pane's observed directory. A relative path is resolved there; `~` and `~/...` are supported. Existing panes keep their directories, while future windows, splits, recovery files, and saves use the new root.

`switch-session` retains the current QUIC connection and client identity while changing the attached live session.

Confirmation accepts `y`/`Y`; `n`, Enter, Escape, or `Ctrl+c` cancels.

`send-keys` accepts ordinary UTF-8 text, named keys such as `Enter`, `Escape`,
`Tab`, `Up`, `Down`, `Left`, `Right`, `Home`, `End`, `Delete`, and `F1` through
`F12`, plus modifier forms such as `C-c` and `M-x`. `-l` sends its arguments
literally. Use `--` when a literal key argument begins with `-`.

In copy mode, `send-keys -X` accepts `scroll-up`, `scroll-down`, `page-up`,
`page-down`, `halfpage-up`, `halfpage-down`, `history-top`, `history-bottom`,
`begin-selection`, `clear-selection`, `copy-selection`,
`copy-selection-and-cancel`, and `cancel`.

`capture-pane` captures the active pane's visible screen by default. `-S` and
`-E` select line ranges; zero is the first visible line, negative values refer
to history, `-S -` starts at the oldest retained history, and `-E -` ends at
the bottom of the visible screen. `-p` prints to stdout; without `-p`, output
is stored in a new or named paste buffer. `-e` includes ANSI style escapes and
`-C` octal-escapes non-printable bytes. `-J` joins terminal-wrapped rows and
`-N` preserves trailing spaces.

`list-panes -a` lists all live panes across all sessions. It cannot be combined
with `-t`; use either a server-wide listing or a targeted listing. Targeted
rows retain the existing behavior. Rows are ordered by session ID, window
display index, and layout order, with pane ID as the deterministic fallback.

`list-panes -F` and `new-session -P -F` share these format variables:

```text
#{session_id}             numeric session ID
#{session_name}           session name, or empty for unnamed sessions
#{session_created}        stable Unix creation timestamp
#{window_index}            window display index
#{pane_id}                 numeric daemon-wide pane ID
#{pane_dead}               0 for currently listable panes
#{pane_current_command}    current command name/basename
#{pane_current_path}       observed current directory or launch directory
#{pane_in_mode}            1 in copy/history mode, otherwise 0
#{pane_index}              numeric pane ID (existing Meja compatibility)
#{pane_width}              pane width in cells
#{pane_height}             pane height in cells
#{pane_in_copy_mode}       same value as pane_in_mode
```

Unknown variables are preserved literally. Pane IDs are daemon-wide,
monotonically increasing, and are never reused after pane or session exit, so
`#{pane_id}` alone is an unambiguous live-pane identity. Project-file pane
references are layout-local and are remapped to fresh live IDs during restore.

Paste buffers are daemon-wide. Buffers created without `-b` receive automatic
names such as `buffer0001`; the newest automatic buffer is used when `-b` is
omitted. Up to 50 automatic buffers are retained, while named buffers persist
until deleted. `paste-buffer` replaces linefeeds with carriage returns by
default, `-r` preserves linefeeds, `-s` selects a separator, `-p` requests
bracketed paste when the application supports it, and `-d` deletes the buffer
after a successful paste. The default `Ctrl+b ]` binding runs `paste-buffer`.

## Targets and in-pane context

External session commands generally require `-t <id-or-name>`. External window targets use:

```text
<session-id-or-name>:<zero-based-window-index>
```

Pane IDs are separate daemon-wide numeric identities. Current session-oriented
`-t` targets continue to select sessions; future pane-specific commands can use
the unique `#{pane_id}` value without a session-local disambiguation step.

Meja injects `MEJA_SOCKET`, `MEJA_SESSION_TARGET`, and `MEJA_PANE_ID` into panes. A plain local command with no explicit `-L`, `-S`, or `-h` automatically targets the current server, session, and originating pane where the command needs pane context:

```sh
meja set-root .
meja rename work
meja new-window
```

Session-producing CLI commands run from a pane can hand the existing client to the new/restored session instead of nesting a client:

```sh
meja new -s experiment
meja restore -t work
meja new -f dev.meja
```

An explicit profile, socket, or host disables this injected context.

The injected session target is session-oriented at the CLI boundary. Internally
the daemon may use a stable group target so a pane created before grouping can
continue to resolve commands after its original session is removed.

---

# Named-session recovery

Only named sessions are persisted. Profile recovery files are stored at:

```text
~/.meja/<profile>/sessions/<name>.session.meja
```

For `-S /path/to/meja.sock`, they are stored under `/path/to/sessions/`. Directories use `0700`; files use `0600`.

Persistence is change-driven rather than timer-based. Meja writes after recoverable structure changes and after stable process observations change a pane's recorded command or directory.

Grouped recovery records use schema 2 metadata while keeping the existing
`<name>.session.meja` location. Each named session records its stable group ID,
active and previous windows, and independent focus/zoom views; version-1 files
restore as singleton groups. Live leases, client instances, output bindings,
transports, and terminal contents are never persisted.

When another view from a group is restored while that group is already live,
the daemon reuses the existing canonical windows, panes, and processes. It
does not launch duplicate panes. Persistence writes are asynchronous, so a
save request does not wait for filesystem I/O in the daemon transaction.

Recovery records include the root, windows, panes, layout, active window/panes, directories, shells, explicit commands, and stable detected foreground commands. They do not contain process memory, terminal contents, scrollback, descriptors, connections, or application state.

Use `attach` while the original session exists. Use `restore -t <name>` after it is gone. Private recovery files are implementation records; use `save` for a normalized project file.

---

# Project `.meja` files

Project files are readable KDL. `save` writes them; `new -f` reads them. Remote commands read/write files on the remote machine and never transfer them automatically.

User-owned files omit private metadata such as session IDs, timestamps, active window, and active pane. Sessions created from project files start in the first window and first pane. Files are limited to 4 MiB.

## Example

```kdl
root "."

window name="editor" {
    layout "main-vertical"

    pane {
        cwd "frontend"
        cmd "npm run dev"
    }

    pane {
        cwd "backend"
        cmd "go run ."
    }
}

window name="tests" {
    pane {
        cmd "go test ./..."
    }
}
```

Current files use top-level `root` and `window` nodes; no `session` wrapper or version declaration is required. A legacy leading `meja` node is tolerated. Unknown nodes are ignored, while duplicate or invalid known values are rejected.

## Nodes

### `root`

Exactly one root is required. It may be absolute, `~`/`~/...`, or relative to the file's directory. It must resolve to an existing directory when the session is created.

### `window`

```kdl
window name="editor" {
    cwd "services"
    layout "tiled"
    pane { /* ... */ }
}
```

A file needs at least one window. `name` is optional. Each window accepts one optional `cwd`, one optional `layout`, and one to eight panes.

### `pane`

```kdl
pane {
    cwd "api"
    shell "/bin/zsh"
    cmd "npm run dev"
    tile x=0 y=0 w=50 h=100
}
```

A pane accepts at most one each of `cwd`, `shell`, `cmd`, and `tile`. With one pane, no layout or tile is needed. Relative window paths use `root`; relative pane paths use the window path.

The shell defaults to the selected user's shell. `cmd` is typed into that interactive shell and is controlled by `--commands=prepare|skip|run`. Shell and command strings may not contain control characters.

## Layouts

Supported named layouts are:

```text
even-horizontal
even-vertical
main-horizontal
main-vertical
tiled
```

For custom layouts, every pane supplies `tile x= y= w= h=` values on a `100 × 100` surface. Values are integers from 0 through 100; widths/heights are positive. Tiles must stay in bounds, not overlap, cover the full surface, and form a recursively splittable tiling.

An unsupported layout name is accepted only when valid tile fallback data exists.

## Save normalization

Save makes the root relative to the output file, removes redundant inherited directories/default shells, uses named presets when recognized, and writes custom layouts as tiles. Output is deterministic, but regeneration does not preserve comments or unknown formatting.

---

# In-session interaction

## Prefix keys

Meja uses `Ctrl+b`. Press it, release it, then press the command key.

| Keys | Behavior |
| --- | --- |
| `Ctrl+b`, `Ctrl+b` | Send literal `Ctrl+b`. |
| `Ctrl+b`, `d` | Detach. |
| `Ctrl+b`, `c` | Create a window. |
| `Ctrl+b`, `Space` | Cycle layouts. |
| `Ctrl+b`, `%` | Split left/right. |
| `Ctrl+b`, `"` | Split top/bottom. |
| `Ctrl+b`, arrows | Focus a pane. |
| `Ctrl+b`, Ctrl+arrows | Resize by one cell. |
| `Ctrl+b`, Alt+arrows | Resize by five cells. |
| `Ctrl+b`, `z` | Toggle zoom. |
| `Ctrl+b`, `{` / `}` | Swap previous/next pane. |
| `Ctrl+b`, `x` | Confirm and close pane. |
| `Ctrl+b`, `[` | Enter history/copy mode. |
| `Ctrl+b`, `:` | Open command prompt. |
| `Ctrl+b`, `n` / `p` / `l` | Next/previous/last window. |
| `Ctrl+b`, `0`–`9` | Select window index. |
| `Ctrl+b`, `,` | Rename window. |
| `Ctrl+b`, `$` | Name/rename session. |

A window supports eight visible panes. Closing the final pane ends the session. Resize arrows repeat without another prefix for 500 ms after a prefixed resize. Zoom preserves the split layout and hidden processes.

## Command and rename prompts

The command prompt accepts shell-like word splitting, quotes, and backslash escapes, but performs no variables, globs, redirection, substitution, or shell execution.

```text
set-root .
rename-window "api server"
resize-pane -R 5
switch-session -t staging
restore-session -t work
```

Type UTF-8 text; Backspace/Delete removes the previous character; `Ctrl+u` clears; Enter submits; Escape/`Ctrl+c` cancels. Cursor movement is not currently bound.

## History and copy

In history mode:

| Keys | Behavior |
| --- | --- |
| arrows or `h`/`j`/`k`/`l` | Move cursor. |
| Page Up/Down | Move 12 rows. |
| `Ctrl+u` / `Ctrl+d` | Move 6 rows. |
| `g` / `G` | Oldest/newest position. |
| Space | Start selection. |
| Enter | Copy selection and exit. |
| `q`, Escape, `Ctrl+c` | Exit without copying. |

Movement after Space extends the selection. Enter without movement copies the cursor cell. Selection is rendered black on yellow and copied with OSC 52; terminal clipboard policy must permit it. Maximum copied data is 1 MiB.

## Mouse

Clicking focuses a pane. When its application is not using mouse tracking, left-drag selects from a frozen history snapshot and copies with OSC 52 on release. Wheel input scrolls history in copy mode; in a normal pane without mouse tracking it is delivered as Up/Down keys.

When an application enables mouse reporting, Meja routes supported press, release, drag/motion, modifier, and wheel events to the application instead of starting selection.

## Terminal input modes

Per-pane routing supports application cursor keys, bracketed paste, focus reporting, classic/SGR mouse encodings, X10/button/drag/motion tracking, and Kitty keyboard flags/event types. Paste remains targeted at the pane active when it began. The server owns prefix handling; the client uses only conservative local prediction for eligible printable input.

---

# Server and remote behavior

## Automatic startup

| Command | Starts missing server |
| --- | --- |
| help, new, restore | yes |
| attach, save, ls, kill-server | no |
| start-server | runs directly |

Meja removes stale owned sockets for commands allowed to start a server.

Socket directories must be owned by the user with exact mode `0700`; sockets and locks use `0600`. An exact socket cannot live directly in a shared directory such as `/tmp`. A file lock prevents duplicate servers.

Session IDs start at 1 for each server lifetime. Detached sessions continue while they have panes. The final pane ending closes the session. Stopping the server ends live sessions but leaves named recovery files. Server shutdown terminates all panes concurrently: it allows one second after `SIGHUP` and PTY closure, one second after `SIGTERM`, and one final second after `SIGKILL`, for a maximum pane-cleanup delay of about three seconds regardless of pane count.

## SSH and QUIC

Remote CLI commands use SSH briefly to forward the command request and receive attach data. The active terminal then connects directly using QUIC/TLS 1.3 to the first available UDP port in `60000-61000`.

The client pins the server public key fingerprint delivered through authenticated SSH, so no user CA configuration is required. Firewalls, VPNs, proxies, NAT, and security groups must permit the direct UDP path.

## Reconnection

On transport loss, the client keeps the last confirmed screen, shows reconnecting status, drops input, and retries with exponential backoff up to two seconds. It resumes after receiving the current layout and full visible state.

Reconnect uses the in-memory resume token for the existing client identity and does not repeat SSH or contact the Unix socket. Session end, server stop, or client replacement produces a terminal reason and clean exit rather than endless reconnect.

---

# Pane environment and diagnostics

Panes start login shells unless the initial pane received an explicit command. The base environment includes:

```text
HOME USER LOGNAME SHELL
TERM=xterm-256color
PATH=<inherited from the server environment when set>
```

`LANG`, `LC_ALL`, and `LC_CTYPE` are copied when set. Meja also injects `MEJA_SOCKET`, `MEJA_SESSION_TARGET`, and `MEJA_PANE_ID` for in-pane context.

Render diagnostics:

```sh
MEJA_DEBUG=1 meja
MEJA_DEBUG_RENDER=1 meja attach -t work
MEJA_DEBUG_LOG=/tmp/meja-render.log meja attach -t work
```

`MEJA_DEBUG_LOG` also enables diagnostics. Without it, output goes to stderr. Boolean values use Go parsing; `1` and `true` are predictable enabled values.

On Linux, low UDP socket-buffer limits may produce a QUIC warning and limit throughput. Administrators may raise `net.core.rmem_max` and `net.core.wmem_max` through normal sysctl configuration.

---

# Private SSH command

```text
meja [-L profile | -S socket-path] __ssh-forward-v1
```

This private, versioned command reads a framed request from stdin, resolves/optionally starts the selected server, forwards the request to its Unix socket, and writes framed output plus optional attach bootstrap data to stdout. It is not intended for interactive use.
