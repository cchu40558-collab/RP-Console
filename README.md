# RP Console

RP Console is the first runnable phase of the Relay Panel central site. It is a standalone Go service that provides:

- A single administrator login.
- AES-GCM encrypted local storage for child-panel central read-only tokens.
- Child-server registration with manual VPS-expiry reminders.
- Protocol-verified probes of a child Relay Panel's central summary and safe line list.
- A dashboard with green/yellow/red status dots, management latency, cumulative line traffic, line count and audit events.
- A read-only child-line view that remains under the RP Console browser route.

It intentionally does **not** yet provide remote line mutation, mTLS enrollment, or remote upgrade/rollback. Those operations must not be exposed before a separate node-side controlled write API exists.

## Server deployment

The next deployment-automation release is `v2.0.19`. It installs only RP Console itself: it does not install Xray or create proxy lines.

Before the first installation, create the Cloudflare DNS record and open TCP `80` and `443` in the cloud-provider firewall. Set Cloudflare SSL/TLS to `Full` temporarily. The installer creates a short-lived self-signed origin certificate so that the administrator can finish TLS setup in the web panel. Run as root or through `sudo`:

```bash
sudo env \
  CONSOLE_REPO_REF=v2.0.19 \
  CONSOLE_DOMAIN=rp-console.wakeup-ai.top \
  bash <(curl -fsSL https://raw.githubusercontent.com/cchu40558-collab/RP-Console/v2.0.19/scripts/install-server.sh)
```

After signing in, open **Site settings**, enter the domain, upload the Cloudflare Origin Certificate and matching private key, then choose **Save and apply**. RP Console validates the pair, atomically replaces only its own Nginx TLS configuration, reloads Nginx, and opens UFW ports `80/443` when UFW is active. It does not expose port `2053`. After the panel reports success, change Cloudflare SSL/TLS to `Full (strict)`.

The installer creates a loopback-only service on `127.0.0.1:2053`, configures Nginx HTTPS, writes root-only credentials to `/root/rp-console-install-result.env`, and provides:

```text
rp-console version | status | logs
sudo rp-console check | restart | update vX.Y.Z | rollback | backups | password
```

The initial installation also requires Google Cloud VPC firewall rules for TCP `80` and `443`. The installer configures UFW only; it cannot change Google Cloud firewall rules.

## Child-panel requirements

The child must run Relay Panel `v2.0.18` or later. In the child panel, open **Settings > Security > Central access**, create a central read-only token, and copy it immediately. The plaintext token is shown only once.

RP Console only calls these token-restricted endpoints:

```text
GET /panel/api/central/capabilities
GET /panel/api/central/summary
GET /panel/api/central/lines
```

When a child uses a management base path, configure that path during registration. For example, `/a1b2c3` produces:

```text
https://child.example:2053/a1b2c3/panel/api/central/summary
```

The central read-only token cannot access ordinary child API routes, and ordinary child API tokens cannot access the central routes.

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

- Bind RP Console to `127.0.0.1:2053` and use Nginx on port 443.
- Put `rp-console.wakeup-ai.top` behind Cloudflare with Full (strict) TLS and Cloudflare Access after the Origin Certificate has been applied.
- Store `CENTRAL_ADMIN_PASSWORD` and `CENTRAL_MASTER_KEY` in a root-readable systemd environment file with mode `0600`.
- Do not change `CENTRAL_MASTER_KEY` after registering servers; doing so makes stored API Tokens unreadable.
- Child management endpoints must use HTTPS and a dedicated central read-only token; mTLS enrollment is a later required phase.
- Set `CENTRAL_ALLOW_PRIVATE_NODES=true` only after intentionally using a private network such as WireGuard.


## Tests

```powershell
go test ./...
go vet ./...
```
