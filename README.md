# Relay Central

Relay Central is the first runnable phase of the Relay Panel central site. It is a standalone Go service that provides:

- A single administrator login.
- Encrypted local storage for child-panel central read-only tokens.
- Child-server registration with manual VPS-expiry reminders.
- Protocol-verified probes of a child Relay Panel's central summary and safe line list.
- A dashboard with green/yellow/red status dots, management latency, cumulative line traffic, line count and audit events.

It intentionally does **not** yet provide remote line mutation, mTLS enrollment, or remote upgrade/rollback. Those operations must not be exposed before a separate node-side controlled write API exists.

## Run locally

Generate a 32-byte data-encryption key:

```powershell
$key = [Convert]::ToBase64String((1..32 | ForEach-Object { Get-Random -Maximum 256 }))
```

Set a password and start the service:

```powershell
$env:CENTRAL_ADMIN_PASSWORD = 'use-a-long-unique-password'
$env:CENTRAL_MASTER_KEY = $key
go run ./cmd/relay-central
```

Open `http://127.0.0.1:2053`.

## Production requirements

- Bind the service to `127.0.0.1:2053` and use Nginx on port 443.
- Put `admin.wakeup-ai.top` behind Cloudflare with Full (strict) TLS and Cloudflare Access.
- Store `CENTRAL_ADMIN_PASSWORD` and `CENTRAL_MASTER_KEY` in a root-readable systemd environment file with mode `0600`.
- Do not change `CENTRAL_MASTER_KEY` after registering servers; doing so makes stored API Tokens unreadable.
- Child management endpoints must use HTTPS. The first phase uses existing child API Tokens; mTLS enrollment is a later required phase.
- Set `CENTRAL_ALLOW_PRIVATE_NODES=true` only after intentionally using a private network such as WireGuard.

## Child-panel requirements for the first phase

The child must expose its existing RP API over HTTPS and allow the central server through its firewall. The central service reads:

```text
GET /panel/api/server/status
GET /panel/api/lines
GET /panel/api/lines/metrics
GET /panel/api/server/getPanelUpdateInfo
```

The configured management base path is prepended before `/panel/api`. For example, a child path `/a1b2c3` becomes:

```text
https://child.example:2053/a1b2c3/panel/api/lines
```

## Tests

```powershell
go test ./...
go vet ./...
```
