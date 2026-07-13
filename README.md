# tali

`tali` is a QUIC remote terminal client. SSH performs remote-user
authentication, agent/password handling, SSH configuration, and host-key
verification; Tali does not inspect `authorized_keys` or implement a second SSH
authentication protocol.

## Commands

Build the two supported executables:

```bash
go build -o bin/tali ./cmd/tali
go build -o bin/tali-ctrl ./cmd/tali-ctrl
```

Connect with:

```bash
./bin/tali user@host
./bin/tali user@host -s 12
./bin/tali --ctrl-path=/opt/tali/bin/tali-ctrl user@host -- /usr/bin/bash -l
```

The local client invokes the installed `ssh` executable to run the quoted
remote command `tali-ctrl start-session` (or `connect-session <id>` for `-s`).
SSH prints no protocol data of its own to the client's stdout: the controller emits exactly one
`TALI_BOOTSTRAP_V1 {json}` record, while diagnostics go to stderr. The JSON
contains a numeric session ID, UDP port, expiring single-use attach token, and
the SHA-256 hash of the daemon certificate's SubjectPublicKeyInfo.

The remote controller supports:

```text
tali-ctrl server
tali-ctrl start-session
tali-ctrl connect-session <numeric-session-id>
tali-ctrl stop-server
```

`start-session` starts the per-Unix-user daemon when necessary. To attach to
an existing session, run `tali user@host -s <numeric-session-id>`; this invokes
`tali-ctrl connect-session <id>` and never starts a missing daemon. Its protected
control socket is `$XDG_RUNTIME_DIR/tali/control.sock`, falling back to
`~/.tali/control.sock`; the socket directory is mode 0700 and the socket is
mode 0600. `connect-session` only performs an RPC and never starts a daemon.
`stop-server` only performs an RPC against the existing control socket;
it cleanly disconnects active clients as if they detached with Ctrl-B, d,
gracefully stops the active daemon, and reports its PID when available. SIGINT
and SIGTERM on the daemon use the same client-disconnect behavior.
Session IDs increase from 1 for the lifetime of a daemon and sessions end with
that daemon.

## Security model

The daemon runs under the SSH-authenticated account, so panes naturally have
that account's UID/GID and environment. No root credential switching is used.
The daemon generates a self-signed TLS 1.3 certificate and chooses a UDP port
in 60000–61000. The client uses an internal `InsecureSkipVerify` setting only
with a mandatory exact SPKI `VerifyConnection` pin from the SSH bootstrap;
there is no genuinely unverified production TLS mode and no CA/certificate/key
setup is required.

The first QUIC management message is the versioned session attachment
`{sessionId, attachToken}`. The token is random, expiry-bound, listener-bound,
constant-time compared, and atomically consumed after a successful match.
After attachment the daemon issues a session-scoped resume credential and
generation. Replacement connections rotate that credential and fence the old
generation.

## Defaults and SSH options

`~/.tali/config` is not required. The current implementation uses the defaults
above. OpenSSH owns SSH config and host verification. `-i` and explicit
`-port` remain compatibility options and are passed to `ssh`; `--ctrl-path`
selects the exact remote controller path and is shell-quoted before execution.

## Terminal behavior and limitations

The existing server-owned terminal/render protocol, pane commands, alternate
screen handling, and layout behavior remain in use. A QUIC disconnect detaches
the client without immediately killing the session; an explicit detach/remote
session exit ends the attached client flow.

Resume credential validation and generation fencing are implemented as the
secure protocol/state foundation. Full automatic client-side reconnect UX
(orange last-contact overlay, input suppression, authoritative snapshot
recovery, and automatic fallback after a live connection drops) is not yet
wired through the existing render loop; the client currently reports a lost
QUIC connection and exits. Manual reconnection with `-s` is supported.
