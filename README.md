# tali

`tali` is a first vertical slice of a QUIC-native remote terminal client and server.

## Architecture

The protocol uses one QUIC connection with ALPN `tali/1` and three application streams:

- one bidirectional management stream
- one client-to-server input stream
- one server-to-client pane output stream

Every application stream starts with a framed stream-opening message. Frames use:

```text
[varint message type][varint payload length][payload]
```

Payloads in this milestone are JSON encoded.

Authentication is application-level and uses the local `ssh-agent`:

1. the client parses `<user>@<hostname>`
2. the client sends the requested username and an `ssh-ed25519` public key
3. the server checks that key against the target account's `~/.ssh/authorized_keys`
4. the server issues a random challenge
5. the client signs a deterministic transcript via `ssh-agent`
6. the server verifies the signature and rejects expired or reused challenges

After authentication, the server allocates a PTY, starts the login shell or requested command as the authenticated Unix account, parses PTY output into a bounded terminal grid, and sends pane snapshots / row updates to the client for ANSI rendering.

## Build

```bash
go build -o bin/tali ./cmd/tali
go build -o bin/tali-server ./cmd/tali-server
```

## Local Certificate Setup

For local testing, create a development CA and server certificate. Example with OpenSSL:

```bash
mkdir -p dev
openssl req -x509 -newkey rsa:4096 -sha256 -days 365 -nodes \
  -keyout dev/ca.key -out dev/ca.crt -subj "/CN=tali-dev-ca"

openssl req -newkey rsa:4096 -nodes \
  -keyout dev/server.key -out dev/server.csr -subj "/CN=localhost"

openssl x509 -req -in dev/server.csr \
  -CA dev/ca.crt -CAkey dev/ca.key -CAcreateserial \
  -out dev/server.crt -days 365 -sha256
```

## SSH Agent Requirement

The client requires `SSH_AUTH_SOCK` and currently supports only `ssh-ed25519` identities available from the local SSH agent.

## Server Privilege Warning

`tali-server` must run with sufficient privilege to switch to the authenticated Unix user by real UID, primary GID, and supplementary groups. Running as an unprivileged account will prevent correct user switching.

## Example

Start the server:

```bash
sudo ./bin/tali-server \
  -listen :4433 \
  -cert ./dev/server.crt \
  -key ./dev/server.key
```

Connect from the client:

```bash
./bin/tali -ca ./dev/ca.crt alice@localhost
./bin/tali -ca ./dev/ca.crt alice@localhost -- /usr/bin/env bash -l
./bin/tali -i ~/.ssh/id_ed25519 -ca ./dev/ca.crt alice@localhost
./bin/tali -ca ./dev/ca.crt myserver
```

## SSH Config Resolution

The client resolves SSH settings from `~/.ssh/config` before dialing. Supported directives:

- `Host`
- `HostName`
- `User`
- `IdentityFile`
- `IdentitiesOnly`
- `Port`

Resolution precedence:

- hostname: SSH `HostName`, then original host argument
- username: explicit `user@host`, then SSH `User`, then local username
- identity file: explicit `-i`, then SSH `IdentityFile` entries in order, then `~/.ssh/id_ed25519`
- port: explicit `-port`, then SSH `Port`, then Tali default `4433`

Identity selection prefers exact configured identities, uses matching keys from `ssh-agent` when available, and otherwise loads the configured private key from disk. Only Ed25519 identities are currently supported.

## Current Limitations

- only one remote pane is implemented
- only a minimal primary-screen terminal model is implemented
- no pane layouts or multiplexer commands
- no alternate screen or scrollback
- no client certificate authentication
- only `ssh-ed25519` keys are accepted
- `authorized_keys` entries with options are rejected
