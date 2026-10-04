# Central Deployment over Tailscale

Run one CLIProxyAPI instance on an always-on Linux host and point every client machine at it.
Credential selection, session stickiness, cooldowns, and usage accounting then live in one place.

## Host configuration

Bind the proxy to the host's Tailscale address and require client API keys. Example
`~/.config/cli-proxy-api/config.yaml` (secrets redacted):

```yaml
config-version: 8
server:
  host: "100.x.y.z"        # the host's Tailscale IP (`tailscale ip -4`)
  port: 8317
management:
  allow-remote: false      # manage from the host itself, or via `ssh -L 8317:100.x.y.z:8317 devbox`
  secret-key: "<management-key>"
access:
  api-keys:
    - "<client-key-mac>"   # one key per machine so usage can be attributed
    - "<client-key-desktop>"
    - "<client-key-devbox>"
routing:
  strategy: "soonest-reset" # burn quota whose window resets soonest first
  session-affinity: true    # never move a bound session while its credential is available
  session-affinity-ttl: "6h"
oauth:
  auth-dir: "~/.cli-proxy-api"
```

Generate client keys with `openssl rand -hex 32`. Tailscale ACLs should restrict port 8317 to
your own devices.

## systemd user unit

`~/.config/systemd/user/cli-proxy-api.service`:

```ini
[Unit]
Description=CLIProxyAPI
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
ExecStart=%h/.local/bin/cli-proxy-api --config %h/.config/cli-proxy-api/config.yaml --no-browser
WorkingDirectory=%h/.config/cli-proxy-api
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

```bash
go build -o ~/.local/bin/cli-proxy-api ./cmd/server
systemctl --user daemon-reload
systemctl --user enable --now cli-proxy-api
loginctl enable-linger "$USER"   # keep running without an active login session
journalctl --user -u cli-proxy-api -f
```

A user unit cannot order itself after the system `tailscaled` service. If the proxy starts before
Tailscale has assigned its address, binding fails and systemd keeps retrying until it succeeds.

With `host` bound to the Tailscale IP, even a browser on the host reaches the proxy through that
IP, so management is never "local". To use the Management Center from your devices, set
`management.allow-remote: true` and rely on the tailnet-only bind plus a strong `secret-key`.

## Client machines

Claude Code (and tools that launch it, such as T3 Code) read these variables. Put them in the
shell profile or in `~/.claude/settings.json` under `"env"`:

```bash
export ANTHROPIC_BASE_URL="http://devbox.<tailnet>.ts.net:8317"
export ANTHROPIC_AUTH_TOKEN="<client-key-for-this-machine>"
```

`ANTHROPIC_AUTH_TOKEN` is sent as `Authorization: Bearer`, which the proxy accepts. Leave
`ANTHROPIC_API_KEY` unset so Claude Code does not prompt to use an API key. Claude Code sends
`X-Claude-Code-Session-Id` on every request, which session affinity uses as the binding key.

Verify from a client:

```bash
curl -s -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" "$ANTHROPIC_BASE_URL/v1/models" | head
```

### Codex CLI

Codex keeps using its own ChatGPT login unless a custom provider is selected. Add to
`~/.codex/config.toml` (Windows: `%USERPROFILE%\.codex\config.toml`):

```toml
model_provider = "devbox"

[model_providers.devbox]
name = "CLIProxyAPI (dev-box)"
base_url = "http://devbox.<tailnet>.ts.net:8317/v1"
env_key = "CLIPROXY_API_KEY"
wire_api = "responses"
supports_websockets = true
```

and export the same per-machine client key as `CLIPROXY_API_KEY`. Desktop apps that launch Codex
(T3 Code, IDEs) usually do not see shell exports; in that case replace `env_key` with
`experimental_bearer_token = "<client-key-for-this-machine>"` in the provider block. For the same
reason, prefer the `env` block of `~/.claude/settings.json` over shell exports for Claude Code. Codex tries the websocket
transport (`GET /v1/responses`) first and falls back to HTTPS. Codex OAuth accounts must be added
to the proxy (Management Center → OAuth Login) for Codex models to be served.

## How selection behaves

- New sessions bind to the available credential whose weekly window resets soonest (5h reset
  breaks ties), read from earlier responses: `anthropic-ratelimit-unified-*` for Claude,
  `x-codex-primary-*` / `x-codex-secondary-*` for Codex (the longer window counts as weekly).
  Credentials never used yet are picked last.
- A bound session stays on its credential until that credential is unavailable (rate-limited
  into cooldown, quota exhausted, disabled), then fails over once and re-binds to the next pick.
- Session bindings are held in memory, so a restart starts every session fresh.
