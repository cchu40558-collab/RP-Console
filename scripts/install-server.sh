#!/usr/bin/env bash
# RP Console installer and in-place upgrader. Use immutable Git tags only.

set -Eeuo pipefail
umask 077

readonly REPOSITORY_URL="https://github.com/cchu40558-collab/RP-Console.git"
readonly REPOSITORY_RAW_URL="https://raw.githubusercontent.com/cchu40558-collab/RP-Console"
readonly APP_USER="rp-console"
readonly APP_DIR="/usr/local/rp-console"
readonly LIB_DIR="/usr/local/lib/rp-console"
readonly ENV_DIR="/etc/rp-console"
readonly ENV_FILE="${ENV_DIR}/rp-console.env"
readonly DATA_DIR="/var/lib/rp-console"
readonly BACKUP_DIR="/var/backups/rp-console"
readonly UNIT_FILE="/etc/systemd/system/rp-console.service"
readonly NGINX_SITE="/etc/nginx/sites-available/rp-console.conf"
readonly NGINX_ENABLED="/etc/nginx/sites-enabled/rp-console.conf"
readonly GO_ROOT="/opt/rp-console-go"
readonly RESULT_FILE="/root/rp-console-install-result.env"
readonly SUDOERS_FILE="/etc/sudoers.d/rp-console-apply"

WORK_DIR=""
UPGRADE_BACKUP=""
MODE="install"
INITIAL_MUTATION=0
APP_USER_CREATED=0

log() {
    printf 'rp-console: %s\n' "$*"
}

die() {
    printf 'rp-console: %s\n' "$*" >&2
    exit 1
}

require_root() {
    [[ "${EUID}" -eq 0 ]] || die "run this script as root (or with sudo)"
}

cleanup_initial_install() {
    systemctl disable --now rp-console.service >/dev/null 2>&1 || true
    rm -f "${UNIT_FILE}" "${NGINX_SITE}" "${NGINX_ENABLED}" "${SUDOERS_FILE}" "${RESULT_FILE}" /usr/local/bin/rp-console
    rm -rf "${APP_DIR}" "${LIB_DIR}" "${ENV_DIR}" "${DATA_DIR}"
    if [[ "${APP_USER_CREATED}" -eq 1 ]]; then
        userdel "${APP_USER}" >/dev/null 2>&1 || true
    fi
    systemctl daemon-reload >/dev/null 2>&1 || true
    nginx -t >/dev/null 2>&1 && systemctl reload nginx >/dev/null 2>&1 || true
}

on_exit() {
    local result="$?"
    trap - EXIT
    if [[ "${result}" -ne 0 ]]; then
        if [[ "${MODE}" == "upgrade" && -n "${UPGRADE_BACKUP}" ]]; then
            printf 'rp-console: upgrade failed; restoring the previous snapshot.\n' >&2
            set +e
            maintenance_restore_snapshot "${UPGRADE_BACKUP}"
            local restore_result="$?"
            set -e
            if [[ "${restore_result}" -ne 0 ]]; then
                printf 'rp-console: automatic restore also failed. Inspect %s.\n' "${UPGRADE_BACKUP}" >&2
            fi
        elif [[ "${MODE}" == "install" && "${INITIAL_MUTATION}" -eq 1 ]]; then
            printf 'rp-console: installation failed; removing incomplete RP Console files.\n' >&2
            set +e
            cleanup_initial_install
            set -e
        fi
    fi
    [[ -z "${WORK_DIR}" ]] || rm -rf "${WORK_DIR}"
    exit "${result}"
}
trap on_exit EXIT

validate_ref() {
    [[ "${CONSOLE_REPO_REF:-}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "CONSOLE_REPO_REF must be an immutable tag such as v2.0.18"
}

validate_domain() {
    [[ "${CONSOLE_DOMAIN:-}" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$ ]] || die "CONSOLE_DOMAIN must be a lowercase fully qualified domain name"
}

validate_admin_password() {
    local value="$1"
    [[ "${value}" =~ ^[A-Za-z0-9._~!@#%^+=:-]{16,256}$ ]] || die "CONSOLE_ADMIN_PASSWORD must contain 16-256 safe non-whitespace characters"
}

ensure_packages() {
    command -v apt-get >/dev/null 2>&1 || die "only apt-based Debian/Ubuntu hosts are currently supported"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y ca-certificates curl git nginx openssl build-essential sudo
}

version_at_least() {
    local actual="$1"
    local wanted="$2"
    [[ "$(printf '%s\n%s\n' "${wanted}" "${actual}" | sort -V | head -n 1)" == "${wanted}" ]]
}

go_version_from_mod() {
    awk '$1 == "go" { print $2; exit }' "${1}/go.mod"
}

ensure_go() {
    local required="$1"
    local candidate=""
    local current=""
    if command -v go >/dev/null 2>&1; then
        candidate="$(command -v go)"
        current="$(${candidate} version | sed -nE 's/^go version go([0-9]+\.[0-9]+(\.[0-9]+)?).*/\1/p')"
    fi
    if [[ -n "${current}" ]] && version_at_least "${current}" "${required}"; then
        GO_BINARY="${candidate}"
        return
    fi

    local arch archive temporary_go
    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) die "unsupported CPU architecture: $(uname -m)" ;;
    esac
    GO_BINARY="${GO_ROOT}/go${required}/bin/go"
    if [[ -x "${GO_BINARY}" ]]; then
        current="$(${GO_BINARY} version | sed -nE 's/^go version go([0-9]+\.[0-9]+(\.[0-9]+)?).*/\1/p')"
        if version_at_least "${current}" "${required}"; then
            return
        fi
    fi

    archive="${WORK_DIR}/go${required}.linux-${arch}.tar.gz"
    log "installing Go ${required} for the RP Console build environment"
    curl --fail --location --silent --show-error --retry 3 -o "${archive}" "https://go.dev/dl/go${required}.linux-${arch}.tar.gz"
    temporary_go="${WORK_DIR}/go"
    tar -C "${WORK_DIR}" -xzf "${archive}"
    [[ -x "${temporary_go}/bin/go" ]] || die "downloaded Go archive is invalid"
    rm -rf "${GO_ROOT}/go${required}"
    mkdir -p -m 0755 "${GO_ROOT}"
    mv "${temporary_go}" "${GO_ROOT}/go${required}"
    GO_BINARY="${GO_ROOT}/go${required}/bin/go"
}

clone_source() {
    git ls-remote --exit-code --tags "${REPOSITORY_URL}" "refs/tags/${CONSOLE_REPO_REF}" >/dev/null || die "tag ${CONSOLE_REPO_REF} does not exist in RP-Console"
    SOURCE_DIR="${WORK_DIR}/source"
    git clone --quiet --depth 1 --branch "${CONSOLE_REPO_REF}" "${REPOSITORY_URL}" "${SOURCE_DIR}"
    [[ "$(git -C "${SOURCE_DIR}" describe --tags --exact-match)" == "${CONSOLE_REPO_REF}" ]] || die "checked out source is not the requested tag"
}

source_version() {
    local source="$1"
    local version=""
    if [[ -f "${source}/internal/config/version" ]]; then
        version="$(tr -d '\r\n' < "${source}/internal/config/version")"
    fi
    if [[ -z "${version}" && -f "${source}/internal/central/central.go" ]]; then
        version="$(sed -nE 's/^[[:space:]]*Version[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "${source}/internal/central/central.go" | head -n 1)"
    fi
    [[ -n "${version}" ]] || die "could not determine RP Console source version"
    printf '%s\n' "${version}"
}

validate_tls_pair() {
    local cert="$1"
    local key="$2"
    [[ -r "${cert}" && -f "${cert}" ]] || die "CONSOLE_TLS_CERT_FILE is not a readable regular file"
    [[ -r "${key}" && -f "${key}" ]] || die "CONSOLE_TLS_KEY_FILE is not a readable regular file"
    openssl x509 -in "${cert}" -noout >/dev/null || die "the TLS certificate is invalid"
    openssl pkey -in "${key}" -noout >/dev/null 2>&1 || die "the TLS private key is invalid"
    local cert_key private_key
    cert_key="$(openssl x509 -in "${cert}" -noout -pubkey | openssl pkey -pubin -outform DER | sha256sum | awk '{print $1}')"
    private_key="$(openssl pkey -in "${key}" -pubout -outform DER | sha256sum | awk '{print $1}')"
    [[ "${cert_key}" == "${private_key}" ]] || die "the TLS certificate and private key do not match"
}

write_environment_file() {
    local password="$1"
    local master_key="$2"
    install -d -m 0750 "${ENV_DIR}"
    cat > "${ENV_FILE}" <<EOF
CENTRAL_ADMIN_PASSWORD=${password}
CENTRAL_MASTER_KEY=${master_key}
CENTRAL_DATA_DIR=${DATA_DIR}
CENTRAL_LISTEN_ADDR=127.0.0.1:2053
CENTRAL_ALLOW_PRIVATE_NODES=false
CENTRAL_PRIVILEGED_APPLY=${LIB_DIR}/apply-site
EOF
    chmod 0600 "${ENV_FILE}"
}

ensure_privileged_apply_setting() {
    local temporary
    temporary="$(mktemp "${ENV_FILE}.XXXXXX")"
    chmod 0600 "${temporary}"
    { grep -v '^CENTRAL_PRIVILEGED_APPLY=' "${ENV_FILE}"; printf 'CENTRAL_PRIVILEGED_APPLY=%s\n' "${LIB_DIR}/apply-site"; } > "${temporary}"
    install -m 0600 "${temporary}" "${ENV_FILE}"
    rm -f "${temporary}"
}

write_initial_result() {
    local password="$1"
    cat > "${RESULT_FILE}" <<EOF
CONSOLE_URL=https://${CONSOLE_DOMAIN}
CONSOLE_VERSION=${SOURCE_VERSION}
CONSOLE_ADMIN_PASSWORD=${password}
EOF
    chmod 0600 "${RESULT_FILE}"
}

write_systemd_unit() {
    cat > "${UNIT_FILE}" <<EOF
[Unit]
Description=RP Console Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${APP_USER}
Group=${APP_USER}
EnvironmentFile=${ENV_FILE}
ExecStart=${APP_DIR}/rp-console
Restart=on-failure
RestartSec=3
# The service can invoke only the exact no-argument helper granted below in
# /etc/sudoers.d/rp-console-apply. Keeping NoNewPrivileges here would block
# that narrowly scoped operation altogether.
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
# The web service writes only its data directory. Its single sudo-whitelisted
# helper additionally needs these fixed paths to replace the site's TLS files,
# Nginx vhost, and its own UFW web-port rules. UFW creates /run/ufw.lock while
# reading or changing rules, so its runtime lock directory must be writable too.
# The web service itself still runs as the unprivileged rp-console user.
ReadWritePaths=${DATA_DIR} ${ENV_DIR} /etc/nginx/sites-available /etc/ufw /run

[Install]
WantedBy=multi-user.target
EOF
    chmod 0644 "${UNIT_FILE}"
}

write_nginx_site() {
    cat > "${NGINX_SITE}" <<EOF
server {
    listen 80;
    listen [::]:80;
    server_name ${CONSOLE_DOMAIN};
    return 301 https://\$host\$request_uri;
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name ${CONSOLE_DOMAIN};

    ssl_certificate ${ENV_DIR}/tls/origin.crt;
    ssl_certificate_key ${ENV_DIR}/tls/origin.key;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_timeout 1d;

    location / {
        proxy_pass http://127.0.0.1:2053;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
    }
}
EOF
    chmod 0644 "${NGINX_SITE}"
}

test_nginx_site_before_enable() {
    local test_config="${WORK_DIR}/nginx-rp-console-test.conf"
    cat > "${test_config}" <<EOF
pid /run/nginx-rp-console-test.pid;
events {}
http {
    include /etc/nginx/mime.types;
    include ${NGINX_SITE};
}
EOF
    nginx -t -c "${test_config}" >/dev/null
}

enable_nginx_site() {
    test_nginx_site_before_enable
    ln -s "${NGINX_SITE}" "${NGINX_ENABLED}"
    if ! nginx -t >/dev/null; then
        rm -f "${NGINX_ENABLED}"
        die "nginx rejected the RP Console site configuration"
    fi
    systemctl reload nginx
}

configure_firewall() {
    if command -v ufw >/dev/null 2>&1 && ufw status | head -n 1 | grep -q 'Status: active'; then
        ufw allow 22/tcp >/dev/null
        ufw allow 80/tcp >/dev/null
        ufw allow 443/tcp >/dev/null
    fi
}

build_binary() {
    local required_go
    required_go="$(go_version_from_mod "${SOURCE_DIR}")"
    [[ -n "${required_go}" ]] || die "go.mod does not specify a Go version"
    ensure_go "${required_go}"
    BUILD_BINARY="${WORK_DIR}/rp-console"
    BUILD_APPLY_BINARY="${WORK_DIR}/rp-console-apply"
    (
        cd "${SOURCE_DIR}"
        "${GO_BINARY}" mod download
        "${GO_BINARY}" build -trimpath -o "${BUILD_BINARY}" ./cmd/relay-central
        "${GO_BINARY}" build -trimpath -o "${BUILD_APPLY_BINARY}" ./cmd/rp-console-apply
    )
    [[ -x "${BUILD_BINARY}" && -x "${BUILD_APPLY_BINARY}" ]] || die "RP Console build did not produce the required executables"
}

install_runtime_files() {
    install -d -m 0755 "${APP_DIR}" "${LIB_DIR}"
	chmod 0755 "${APP_DIR}"
    install -m 0755 "${BUILD_BINARY}" "${APP_DIR}/rp-console.new"
    mv -f "${APP_DIR}/rp-console.new" "${APP_DIR}/rp-console"
	chmod 0755 "${APP_DIR}/rp-console"
    printf '%s\n' "${SOURCE_VERSION}" > "${APP_DIR}/VERSION.new"
    chmod 0644 "${APP_DIR}/VERSION.new"
    mv -f "${APP_DIR}/VERSION.new" "${APP_DIR}/VERSION"
	chmod 0644 "${APP_DIR}/VERSION"
    install -m 0700 "${SOURCE_DIR}/scripts/maintenance.sh" "${LIB_DIR}/maintenance.sh"
    install -m 0750 "${BUILD_APPLY_BINARY}" "${LIB_DIR}/apply-site"
}

write_sudoers_rule() {
    local temporary
    temporary="$(mktemp)"
    printf '%s ALL=(root) NOPASSWD: %s ""\n' "${APP_USER}" "${LIB_DIR}/apply-site" > "${temporary}"
    chmod 0440 "${temporary}"
    visudo -cf "${temporary}" >/dev/null || die "generated sudo rule is invalid"
    install -m 0440 "${temporary}" "${SUDOERS_FILE}"
    rm -f "${temporary}"
}

create_bootstrap_certificate() {
    install -d -m 0700 "${ENV_DIR}/tls"
    openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 14 \
        -keyout "${ENV_DIR}/tls/origin.key" \
        -out "${ENV_DIR}/tls/origin.crt" \
        -subj "/CN=${CONSOLE_DOMAIN}" >/dev/null 2>&1
    chmod 0600 "${ENV_DIR}/tls/origin.key"
    chmod 0644 "${ENV_DIR}/tls/origin.crt"
}

write_management_command() {
    cat > /usr/local/bin/rp-console <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

readonly APP_DIR="/usr/local/rp-console"
readonly ENV_DIR="/etc/rp-console"
readonly ENV_FILE="${ENV_DIR}/rp-console.env"
readonly MAINTENANCE="/usr/local/lib/rp-console/maintenance.sh"
readonly REPOSITORY_RAW_URL="https://raw.githubusercontent.com/cchu40558-collab/RP-Console"

die() { printf 'rp-console: %s\n' "$*" >&2; exit 1; }
require_root() { [[ "${EUID}" -eq 0 ]] || die "this operation must be run with sudo"; }
installed_version() { [[ -f "${APP_DIR}/VERSION" ]] || die "RP Console is not installed"; tr -d '\r\n' < "${APP_DIR}/VERSION"; }
valid_ref() { [[ "${1:-}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; }

health_check() {
    local expected="$1" body
    systemctl is-active --quiet rp-console.service || return 1
    body="$(curl --fail --silent --show-error --max-time 5 http://127.0.0.1:2053/healthz)" || return 1
    grep -Eq '"status"[[:space:]]*:[[:space:]]*"ok"' <<<"${body}" || return 1
    grep -Eq '"version"[[:space:]]*:[[:space:]]*"'"${expected}"'"' <<<"${body}"
}

print_check() {
    local failures=0 version output
    version="$(installed_version)"
    printf 'Version: %s\n' "${version}"
    if health_check "${version}"; then printf 'PASS service and health endpoint\n'; else printf 'FAIL service or health endpoint\n'; failures=1; fi
    if nginx -t >/dev/null; then printf 'PASS nginx configuration\n'; else printf 'FAIL nginx configuration\n'; failures=1; fi
    output="$(ss -lnt)"
    if grep -Eq '127\.0\.0\.1:2053|\[::1\]:2053' <<<"${output}" && ! grep -Eq '(0\.0\.0\.0:2053|\[::\]:2053|\*:2053)' <<<"${output}"; then printf 'PASS RP Console listens only on loopback\n'; else printf 'FAIL loopback-only listener check\n'; failures=1; fi
    if grep -Eq '(:|\])443([[:space:]]|$)' <<<"${output}"; then printf 'PASS HTTPS listener\n'; else printf 'FAIL HTTPS listener\n'; failures=1; fi
    if [[ -f "${ENV_DIR}/tls/origin.crt" && -f "${ENV_DIR}/tls/origin.key" && "$(stat -c '%a' "${ENV_DIR}/tls/origin.key")" == 600 && "$(stat -c '%a' "${ENV_FILE}")" == 600 ]]; then printf 'PASS TLS and environment file permissions\n'; else printf 'FAIL TLS or environment file permissions\n'; failures=1; fi
    if command -v ufw >/dev/null 2>&1 && ufw status | head -n 1 | grep -q 'Status: active'; then
        output="$(ufw status)"
        if grep -qE '22/tcp.*ALLOW' <<<"${output}" && grep -qE '80/tcp.*ALLOW' <<<"${output}" && grep -qE '443/tcp.*ALLOW' <<<"${output}" && ! grep -qE '2053/tcp.*ALLOW' <<<"${output}"; then printf 'PASS UFW public-port rules\n'; else printf 'FAIL UFW public-port rules\n'; failures=1; fi
    else
        printf 'PASS UFW inactive or unavailable\n'
    fi
    return "${failures}"
}

change_password() {
    require_root
    local first second temporary
    read -r -s -p 'New RP Console administrator password: ' first; printf '\n'
    read -r -s -p 'Confirm administrator password: ' second; printf '\n'
    [[ "${first}" == "${second}" ]] || die "password confirmation does not match"
    [[ "${first}" =~ ^[A-Za-z0-9._~!@#%^+=:-]{16,256}$ ]] || die "password must contain 16-256 safe non-whitespace characters"
    temporary="$(mktemp "${ENV_FILE}.XXXXXX")"
    chmod 0600 "${temporary}"
    { grep -v '^CENTRAL_ADMIN_PASSWORD=' "${ENV_FILE}"; printf 'CENTRAL_ADMIN_PASSWORD=%s\n' "${first}"; } > "${temporary}"
    install -m 0600 "${temporary}" "${ENV_FILE}"
    rm -f "${temporary}"
    unset first second
    systemctl restart rp-console.service
    health_check "$(installed_version)" || die "password was saved but the service health check failed"
    rm -f /root/rp-console-install-result.env
    printf 'RP Console administrator password updated.\n'
}

update() {
    require_root
    local ref="${1:-}" temporary
    valid_ref "${ref}" || die "usage: rp-console update vX.Y.Z"
    temporary="$(mktemp)"
    trap 'rm -f "${temporary}"' RETURN
    curl --fail --location --silent --show-error --retry 3 -o "${temporary}" "${REPOSITORY_RAW_URL}/${ref}/scripts/install-server.sh"
    CONSOLE_UPGRADE=true CONSOLE_REPO_REF="${ref}" bash "${temporary}"
}

case "${1:-}" in
    version) installed_version ;;
    status) printf 'RP Console v%s\n' "$(installed_version)"; systemctl status rp-console.service --no-pager ;;
    logs) journalctl -u rp-console.service -n 100 --no-pager ;;
    check) require_root; print_check ;;
    restart) require_root; systemctl restart rp-console.service; health_check "$(installed_version)" || die "service health check failed after restart"; printf 'RP Console restarted.\n' ;;
    update) shift; update "${1:-}" ;;
    rollback) require_root; exec "${MAINTENANCE}" rollback ;;
    backups) require_root; exec "${MAINTENANCE}" backups ;;
    password) change_password ;;
    *)
        cat <<'USAGE'
Usage: rp-console <command>
  version | status | logs | check | restart
  update vX.Y.Z | rollback | backups | password
USAGE
        exit 1
        ;;
esac
EOF
    chmod 0755 /usr/local/bin/rp-console
}

start_and_verify_service() {
    systemctl daemon-reload
    systemctl enable rp-console.service >/dev/null
    systemctl restart rp-console.service
    maintenance_health_check "${SOURCE_VERSION}" && return 0
    maintenance_print_service_diagnostics
    die "RP Console did not pass its local health check"
}

initial_install() {
    validate_domain
    [[ ! -e "${NGINX_SITE}" && ! -L "${NGINX_ENABLED}" ]] || die "an RP Console nginx site already exists; use CONSOLE_UPGRADE=true instead"
    ! id "${APP_USER}" >/dev/null 2>&1 || die "the ${APP_USER} user already exists; use CONSOLE_UPGRADE=true after verifying the existing installation"

    local password confirmation master_key
    if [[ -n "${CONSOLE_ADMIN_PASSWORD:-}" ]]; then
        validate_admin_password "${CONSOLE_ADMIN_PASSWORD}"
        password="${CONSOLE_ADMIN_PASSWORD}"
    else
        [[ -t 0 ]] || die "set CONSOLE_ADMIN_PASSWORD for a non-interactive first installation"
        read -r -s -p 'RP Console administrator password: ' password; printf '\n'
        read -r -s -p 'Confirm RP Console administrator password: ' confirmation; printf '\n'
        [[ "${password}" == "${confirmation}" ]] || die "administrator password confirmation does not match"
        validate_admin_password "${password}"
        unset confirmation
    fi
    master_key="$(openssl rand -base64 32 | tr -d '\n')"

    INITIAL_MUTATION=1
    useradd --system --user-group --home-dir "${DATA_DIR}" --shell /usr/sbin/nologin "${APP_USER}"
    APP_USER_CREATED=1
    install -d -m 0700 -o "${APP_USER}" -g "${APP_USER}" "${DATA_DIR}"
    create_bootstrap_certificate
    write_environment_file "${password}" "${master_key}"
    unset master_key
    write_systemd_unit
    write_nginx_site
    install_runtime_files
    write_sudoers_rule
    write_management_command
    start_and_verify_service
    enable_nginx_site
    configure_firewall
    maintenance_health_check "${SOURCE_VERSION}" || die "health check failed after nginx configuration"
    maintenance_nginx_check
    write_initial_result "${password}"
    unset password
    log "Initial administrator credentials were saved to ${RESULT_FILE} (root-readable only)."
    log "The site is using a temporary self-signed origin certificate. Upload the Cloudflare Origin Certificate in RP Console > Site settings, then switch Cloudflare to Full (strict)."
}

upgrade_install() {
    [[ -x "${APP_DIR}/rp-console" && -f "${APP_DIR}/VERSION" && -f "${ENV_FILE}" && -x "${LIB_DIR}/maintenance.sh" ]] || die "an existing complete RP Console installation is required for upgrade"
	local installed
	installed="$(tr -d '\r\n' < "${APP_DIR}/VERSION")"
	maintenance_health_check "${installed}" || die "the currently installed RP Console is not healthy; repair it before upgrading"
	maintenance_nginx_check || die "the current Nginx configuration is invalid; repair it before upgrading"
    UPGRADE_BACKUP="$(maintenance_snapshot_current "pre-update-${CONSOLE_REPO_REF}")"
    install_runtime_files
    ensure_privileged_apply_setting
    write_sudoers_rule
    write_systemd_unit
    write_management_command
    start_and_verify_service
    maintenance_nginx_check
    maintenance_prune_backups
}

main() {
    require_root
    validate_ref
    if [[ "${CONSOLE_UPGRADE:-false}" == "true" ]]; then
        MODE="upgrade"
    elif [[ "${CONSOLE_UPGRADE:-false}" != "false" ]]; then
        die "CONSOLE_UPGRADE must be true when supplied"
    fi
    WORK_DIR="$(mktemp -d /tmp/rp-console-install.XXXXXX)"
    ensure_packages
    clone_source
    SOURCE_VERSION="$(source_version "${SOURCE_DIR}")"
    [[ "v${SOURCE_VERSION}" == "${CONSOLE_REPO_REF}" ]] || die "source version ${SOURCE_VERSION} does not match requested tag ${CONSOLE_REPO_REF}"
    source "${SOURCE_DIR}/scripts/maintenance.sh"
    build_binary
    if [[ "${MODE}" == "upgrade" ]]; then
        upgrade_install
    else
        initial_install
    fi
    log "RP Console v${SOURCE_VERSION} ${MODE} completed successfully."
}

main "$@"
