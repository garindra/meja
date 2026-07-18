# meja

`meja` is a local and remote terminal multiplexer transported over QUIC. A
single executable contains the interactive client, per-user server, and the
small SSH control interface.

SSH performs remote-user authentication, agent/password handling, SSH
configuration, and host-key verification. Meja does not inspect
`authorized_keys` or implement a second SSH authentication protocol.

## Build

Build the single supported executable:

```bash
go build -o bin/meja .
```

Install the appropriate build as `meja` locally and on each remote host.

Install the latest version directly with Go:

```bash
go install github.com/garindra/meja@latest
```

## Commands

Start a new local session:

```bash
meja
meja new
meja new -s work
meja -L dev
```

Connect to a new remote session using a hostname, `user@host`, or an OpenSSH
config alias:

```bash
meja -h prod
meja -h prod new -- /usr/bin/bash -l
meja new -h user@host
meja new -s work -h prod
meja -L dev new -h prod
meja new -r /srv/app -h prod -- /usr/bin/bash -l
```

`-h` (or `--host`) is the SSH transport option. It accepts a hostname,
`user@host`, or OpenSSH alias, so command arguments never need to be parsed by
the client to discover where they should be sent:

```bash
meja new -h server
meja new -i ~/.ssh/prod_ed25519 -h prod
```

Attach to an existing session by numeric ID or name. Omitting the host selects
the local server:

```bash
meja attach -t 12
meja attach -t work
meja a -t 12 -h prod
meja -L dev a -t work -h prod
```

List local or remote sessions:

```bash
meja ls
meja ls -h prod
meja -L dev ls -h prod
```

While attached, press `Ctrl+b`, then `:` and run
`switch-session -t <session-id-or-name>` to move the same client connection to
another live session.

The list is headed `Active Sessions` and shows each session's numeric ID,
name (or `<unnamed>`), and whether a client is currently attached.

Run or stop the local per-user server explicitly:

```bash
meja start-server
meja kill-server
meja -L dev start-server
meja -L dev kill-server
```

## Servers and sockets

Each socket identifies an isolated Meja server process with its own sessions,
session-ID sequence, certificate, and shared QUIC listener. `-L` selects a
named profile and `-S` selects an exact socket path. They are global, mutually
exclusive transport options. Transport options may appear anywhere before
`--` and are removed before the command argv is forwarded:

```bash
meja -L work
meja -L work attach -t 3
meja -L work new -s work -h prod
meja -L work kill-server

meja -S /home/alice/run/meja.sock
meja -S /home/alice/run/meja.sock kill-server
```

With no selector, Meja uses the `default` profile. Named profiles resolve to
`~/.meja/<profile>/meja.sock`, so the default socket is
`~/.meja/default/meja.sock`. Profile names may use letters, digits, `.`, `_`,
and `-`. Exact `-S` paths must be absolute. For a remote command the profile or
path is resolved on the remote host.

Socket directories created by Meja have mode 0700 and sockets have mode 0600.
Meja never changes the permissions of an existing socket parent. An existing
parent must already be owned by the current user with mode 0700, so a socket
cannot be placed directly in a shared directory such as `/tmp`.

Commands that create or restore a session start the selected server if its
socket is missing or stale. `attach`, `ls`, and `kill-server` require a running
server. A foreground `start-server` and an automatically detached server use the
same profile selector. A per-socket lifetime lock prevents two server processes
from owning the same profile. A foreground server logs
`meja server: session <id> attached` for each successful client attachment,
including reconnects and reattachments.

`-h` selects the SSH host, `-i` selects an SSH identity, `--port` selects the
SSH port, and `--remote-path` selects the exact remote `meja` executable. These
transport options may appear anywhere before `--`. The default remote path is
`meja`.

Client render diagnostics are enabled through environment variables. Set
`MEJA_DEBUG=1` to enable all available diagnostics or
`MEJA_DEBUG_RENDER=1` to enable render diagnostics specifically. Diagnostics
are written to stderr unless `MEJA_DEBUG_LOG` names a file; setting that path
also enables render diagnostics:

```bash
MEJA_DEBUG_RENDER=1 meja
MEJA_DEBUG_LOG=/tmp/meja-render.log meja attach -t work
```

`meja new -r <directory>` (or `--root`) sets the session's absolute root and
the working directory for its initial pane and all later windows and splits.
The root is resolved on the target machine.
Quote a remote home-relative path so the local shell does not expand it first:

```bash
meja new -r '~/projects/app' -h prod
```

The command following `--` applies only to the initial pane. Later panes start
the target user's shell in the session root.
When `-r` is omitted, a local session inherits the invoking process's current
directory; a remote session starts in the remote user's home directory.

From the command prompt, `set-root [path]` changes the session root for future
windows and panes. With no path it uses the active pane's current directory;
existing panes are not moved.

## SSH command forwarding

For a remote command, the local client invokes the installed `ssh` executable
with one private, versioned forwarding command:

```text
meja -L <profile> __ssh-forward-v1
meja -S <socket-path> __ssh-forward-v1
```

The user's command arguments travel in a framed request on SSH stdin. The
remote process resolves the selected Unix socket, starts the server if needed,
and forwards the request without interpreting the command. Framed stdout,
stderr, attach-bootstrap, and exit responses return on SSH stdout.

The bootstrap JSON contains a UDP port, expiring single-use attach token, and
the SHA-256 hash of the daemon certificate's SubjectPublicKeyInfo. The attach
token is already bound to its target session, so no session ID is exposed.

Local connections skip SSH entirely. They send the same command request directly
to the protected Unix command socket and connect to the QUIC server through
`127.0.0.1`. Once attached, local and remote reconnects go directly to the
daemon's QUIC listener without consulting SSH or the command socket again.

The protected command socket is selected by `-L` or `-S`; the socket directory is
mode 0700 and the socket is mode 0600. Session IDs increase from 1 for the
lifetime of a daemon. Session names are unique within one server/socket. A
session is destroyed when its last pane exits. The daemon's QUIC listener and
UDP port remain available to its other sessions and client instances.

`meja kill-server` cleanly disconnects active clients as if they detached,
gracefully stops the active daemon, and reports its PID when available. SIGINT
and SIGTERM on a foreground daemon use the same client-disconnect behavior.

## Security model

The daemon runs under the SSH-authenticated account, so panes naturally have
that account's UID/GID and environment. No root credential switching is used.
The daemon generates a self-signed TLS 1.3 certificate and binds one UDP port
in 60000–61000 for all of its client instances and sessions. The client uses
an internal `InsecureSkipVerify` setting only
with a mandatory exact SPKI `VerifyConnection` pin from the bootstrap; there
is no genuinely unverified production TLS mode and no CA/certificate/key setup
is required.

The first QUIC management message is the versioned session attachment
`{attachToken}`. The token is random, expiry-bound, daemon-bound,
constant-time compared, and atomically consumed after a successful match. A
successful attachment creates a daemon-owned client instance for that running
Meja process and returns its reconnect token. The reconnect credential remains
stable for the client-process lifetime and is kept only in memory on both
sides; the client never needs a daemon-side client-instance or session ID.

Each client instance is assigned to at most one session, and each session is
assigned to at most one client instance. A fresh SSH-authenticated `attach`
creates a distinct client instance and takes ownership of its requested
session; a displaced live client shows the takeover reason briefly in its
status bar and exits cleanly. When QUIC closes, the live client-instance object
is discarded immediately. Its small reconnect-token and session-assignment
record remains in daemon memory indefinitely, allowing a later reconnect to
rebuild the client instance without SSH.

The daemon owns client-instance assignment and asks a session to attach or
detach it. The client instance owns the QUIC connection and its explicit
management, input, status, and pane-output streams; the attached session reads
those endpoints directly for input dispatch and graceful output handoff. A
session never discovers its target dynamically from an incoming frame.

Meja uses 1200-byte initial QUIC packets so the handshake fits paths with a
1280-byte IP MTU without depending on fragmentation.

On Linux, quic-go may warn when the kernel limits its UDP socket buffers below
the preferred size. This does not prevent Meja from running, but it can limit
throughput on fast connections. An administrator can raise the live limits:

```bash
sudo sysctl -w net.core.rmem_max=7500000
sudo sysctl -w net.core.wmem_max=7500000
```

Persist those values through the system's sysctl configuration if desired.

## Terminal behavior

The server owns terminal emulation, pane state, layout, and rendering. A QUIC
disconnect detaches the client without immediately killing the session; an
explicit detach or remote session exit ends the attached client flow.

Each connected client instance owns nine server-to-client unidirectional
display streams. The first server stream is permanently bound to the one-row
status surface; the remaining eight are movable pane render slots. Stream
roles come from their QUIC stream ordinals, and every surface uses the same
display-command codec.

Press `Ctrl+b`, then `$` to rename the current session using the status-bar
prompt. Press `Ctrl+b`, then `,` to rename the current window.

After a live QUIC connection drops, the client keeps the last confirmed
terminal contents, replaces the client-visible status bar with an orange
reconnecting indicator with a `Press Ctrl+c to exit` hint, and drops other
input while disconnected. It first retries
the pinned daemon endpoint using its in-memory client-instance reconnect
credential. The reconnect loop never returns to the local command socket or
SSH. A rejected or superseded client instance shows the terminal reason in the
status bar briefly and exits cleanly. Input resumes only after the server's
layout, status bar, and full visible-pane renders have been applied.
