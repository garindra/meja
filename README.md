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
meja prod
meja prod -- /usr/bin/bash -l
meja new user@host
meja new -s work prod
meja -L dev new prod
meja new -c /srv/app prod -- /usr/bin/bash -l
```

An unrecognized first word is treated as a remote target, making `meja prod`
the shorthand for `meja new prod`. The words `new`, `attach`, `a`, `ls`,
`server`, `version`, and `help` are reserved commands. Use the explicit form
for an SSH host alias with one of those names, or whenever connection-specific
flags are needed:

```bash
meja new server
meja new -i ~/.ssh/prod_ed25519 prod
```

Attach to an existing session by numeric ID or name. Omitting the host selects
the local server:

```bash
meja attach -t 12
meja attach -t work
meja a -t 12 prod
meja -L dev a -t work prod
```

List local or remote sessions:

```bash
meja ls
meja ls prod
meja -L dev ls prod
```

The list is headed `Active Sessions` and shows each session's numeric ID,
name (or `<unnamed>`), and whether a client is currently attached.

Run or stop the local per-user server explicitly:

```bash
meja server run
meja server stop
meja -L dev server run
meja -L dev server stop
```

## Servers and sockets

Each socket identifies an isolated Meja server process with its own sessions,
session-ID sequence, QUIC listener, and certificate. `-L` selects a named
profile and `-S` selects an exact socket path. They are global, mutually
exclusive options and must appear before the command:

```bash
meja -L work
meja -L work attach -t 3
meja -L work new -s work prod
meja -L work server stop

meja -S /home/alice/run/meja.sock
meja -S /home/alice/run/meja.sock server stop
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

Commands that create a session start the selected server if its socket is
missing or stale. `attach`, `ls`, and `server stop` never start a missing
server. A foreground `server run` and an automatically detached server use the
same profile selector. A per-socket lifetime lock prevents two server processes
from owning the same profile. A foreground server logs
`meja server: session <id> attached` for each successful client attachment,
including reconnects and reattachments.

Connection flags belong before the host. `-i` selects an SSH identity,
`--port` selects the SSH port, and `--remote-path` selects the exact remote
`meja` executable. The default remote path is `meja`.

Client render diagnostics are enabled through environment variables. Set
`MEJA_DEBUG=1` to enable all available diagnostics or
`MEJA_DEBUG_RENDER=1` to enable render diagnostics specifically. Diagnostics
are written to stderr unless `MEJA_DEBUG_LOG` names a file; setting that path
also enables render diagnostics:

```bash
MEJA_DEBUG_RENDER=1 meja
MEJA_DEBUG_LOG=/tmp/meja-render.log meja attach -t work
```

`meja new -c <directory>` (or `--cwd`) sets the session's starting directory
for its initial pane and all later windows and splits. The directory is
resolved on the target machine and must be absolute or begin with `~/`.
Quote a remote home-relative path so the local shell does not expand it first:

```bash
meja new -c '~/projects/app' prod
```

The command following `--` applies only to the initial pane. Later panes start
the target user's shell in the session's starting directory.
When `-c` is omitted, a local session inherits the invoking process's current
directory; a remote session starts in the remote user's home directory.

## SSH bootstrap

For a remote connection, the local client invokes the installed `ssh`
executable with one of these private, versioned remote commands:

```text
meja __control-v1 start-session
meja __control-v1 start-session <session-name>
meja __control-v1 connect-session <session-id-or-name>
meja __control-v1 list-sessions
meja -L <profile> __control-v1 <operation>
meja -S <socket-path> __control-v1 <operation>
```

The start/connect operations emit exactly one `MEJA_BOOTSTRAP_V1 {json}`
record. The list operation emits exactly one `MEJA_SESSION_LIST_V1 {json}`
record. Diagnostics go to stderr. These commands are a machine interface, not
the user-facing session-management interface.

The bootstrap JSON contains a numeric session ID, UDP port, expiring
single-use attach token, and the SHA-256 hash of the daemon certificate's
SubjectPublicKeyInfo.

Local connections skip SSH entirely. They obtain the same bootstrap directly
from the protected Unix control socket and connect to the QUIC server through
`127.0.0.1`. Local reconnects also use the control socket directly.

`start-session` starts the per-Unix-user daemon when necessary.
`connect-session` only performs an RPC and never starts a missing daemon. The
protected control socket is selected by `-L` or `-S`; the socket directory is
mode 0700 and the socket is mode 0600. Session IDs increase from 1 for the
lifetime of a daemon. Session names are unique within one server/socket. A
session is destroyed when its last pane exits.

`meja server stop` cleanly disconnects active clients as if they detached,
gracefully stops the active daemon, and reports its PID when available. SIGINT
and SIGTERM on a foreground daemon use the same client-disconnect behavior.

## Security model

The daemon runs under the SSH-authenticated account, so panes naturally have
that account's UID/GID and environment. No root credential switching is used.
The daemon generates a self-signed TLS 1.3 certificate and chooses a UDP port
in 60000–61000. The client uses an internal `InsecureSkipVerify` setting only
with a mandatory exact SPKI `VerifyConnection` pin from the bootstrap; there
is no genuinely unverified production TLS mode and no CA/certificate/key setup
is required.

The first QUIC management message is the versioned session attachment
`{sessionId, attachToken}`. The token is random, expiry-bound, listener-bound,
constant-time compared, and atomically consumed after a successful match.
After attachment the daemon issues a session-scoped resume credential and
generation. Replacement connections rotate that credential and fence the old
generation.

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

Each connection has nine server-to-client unidirectional display streams. The
first server stream is permanently bound to the one-row status surface; the
remaining eight are movable pane render slots. Stream roles come from their
QUIC stream ordinals, and every surface uses the same display-command codec.

Press `Ctrl+B`, then `$` to rename the current session using the status-bar
prompt. Press `Ctrl+B`, then `,` to rename the current window.

After a live QUIC connection drops, the client keeps the last confirmed
terminal contents, replaces the client-visible status bar with an orange
reconnecting indicator, and drops input while disconnected. It first retries
the pinned QUIC resume credential. If that fails, it obtains a fresh
single-use attach token through the local control socket or the versioned SSH
control command, as appropriate. Input resumes only after the server's layout,
status bar, and full visible-pane renders have been applied.
