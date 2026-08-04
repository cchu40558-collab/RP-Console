#!/usr/bin/env bash
# Shared local maintenance functions for a deployed RP Console instance.
# This file is installed with mode 0700 and is only invoked by root actions.

set -Eeuo pipefail
umask 077

RP_CONSOLE_APP_DIR="${RP_CONSOLE_APP_DIR:-/usr/local/rp-console}"
RP_CONSOLE_LIB_DIR="${RP_CONSOLE_LIB_DIR:-/usr/local/lib/rp-console}"
RP_CONSOLE_ENV_DIR="${RP_CONSOLE_ENV_DIR:-/etc/rp-console}"
RP_CONSOLE_DATA_DIR="${RP_CONSOLE_DATA_DIR:-/var/lib/rp-console}"
RP_CONSOLE_BACKUP_DIR="${RP_CONSOLE_BACKUP_DIR:-/var/backups/rp-console}"
RP_CONSOLE_UNIT_FILE="${RP_CONSOLE_UNIT_FILE:-/etc/systemd/system/rp-console.service}"
RP_CONSOLE_NGINX_SITE="${RP_CONSOLE_NGINX_SITE:-/etc/nginx/sites-available/rp-console.conf}"
RP_CONSOLE_NGINX_ENABLED="${RP_CONSOLE_NGINX_ENABLED:-/etc/nginx/sites-enabled/rp-console.conf}"
RP_CONSOLE_COMMAND="${RP_CONSOLE_COMMAND:-/usr/local/bin/rp-console}"
RP_CONSOLE_SUDOERS="${RP_CONSOLE_SUDOERS:-/etc/sudoers.d/rp-console-apply}"

maintenance_die() {
    printf 'rp-console: %s\n' "$*" >&2
    exit 1
}

maintenance_require_root() {
    [[ "${EUID}" -eq 0 ]] || maintenance_die "this operation must be run as root"
}

maintenance_version() {
    [[ -f "${RP_CONSOLE_APP_DIR}/VERSION" ]] || return 1
    tr -d '\r\n' < "${RP_CONSOLE_APP_DIR}/VERSION"
}

maintenance_health_check() {
    local expected_version="${1:-}"
    local body attempt

    for attempt in {1..20}; do
        if systemctl is-active --quiet rp-console.service; then
            body="$(curl --fail --silent --show-error --max-time 2 http://127.0.0.1:2053/healthz 2>/dev/null || true)"
            if grep -Eq '"status"[[:space:]]*:[[:space:]]*"ok"' <<<"${body}"; then
                if [[ -z "${expected_version}" ]] || grep -Eq '"version"[[:space:]]*:[[:space:]]*"'"${expected_version}"'"' <<<"${body}"; then
                    return 0
                fi
            fi
        fi
        sleep 1
    done
    return 1
}

maintenance_nginx_check() {
    nginx -t >/dev/null
}

maintenance_print_service_diagnostics() {
    printf '%s\n' 'rp-console: service diagnostics follow:' >&2
    systemctl status rp-console.service --no-pager -l >&2 || true
    journalctl -u rp-console.service -n 80 --no-pager -l >&2 || true
    ss -lntp >&2 || true
}

maintenance_copy_if_present() {
    local source="$1"
    local target="$2"
    if [[ -e "${source}" || -L "${source}" ]]; then
        cp -a "${source}" "${target}"
    fi
}

maintenance_snapshot_current() {
    maintenance_require_root
    local label="$1"
    local stamp destination
    stamp="$(date -u +%Y%m%dT%H%M%SZ)-${label}"
    destination="${RP_CONSOLE_BACKUP_DIR}/${stamp}"
    mkdir -p -m 0700 "${RP_CONSOLE_BACKUP_DIR}"
    [[ ! -e "${destination}" ]] || maintenance_die "backup destination already exists"
    mkdir -m 0700 "${destination}"

    maintenance_copy_if_present "${RP_CONSOLE_APP_DIR}" "${destination}/app"
    maintenance_copy_if_present "${RP_CONSOLE_LIB_DIR}" "${destination}/lib"
    maintenance_copy_if_present "${RP_CONSOLE_ENV_DIR}" "${destination}/etc"
    maintenance_copy_if_present "${RP_CONSOLE_DATA_DIR}" "${destination}/data"
    maintenance_copy_if_present "${RP_CONSOLE_UNIT_FILE}" "${destination}/rp-console.service"
    maintenance_copy_if_present "${RP_CONSOLE_NGINX_SITE}" "${destination}/rp-console.conf"
    maintenance_copy_if_present "${RP_CONSOLE_COMMAND}" "${destination}/rp-console-command"
    maintenance_copy_if_present "${RP_CONSOLE_SUDOERS}" "${destination}/rp-console-apply.sudoers"
    if [[ -L "${RP_CONSOLE_NGINX_ENABLED}" ]]; then
        : > "${destination}/nginx-enabled"
    fi
    printf '%s\n' "${label}" > "${destination}/LABEL"
    # The backup root is private. Preserve the original executable modes inside
    # it so restoring a snapshot cannot make the service binary root-only.
    printf '%s\n' "${destination}"
}

maintenance_restore_snapshot() {
    maintenance_require_root
    local backup="$1"
    [[ -d "${backup}" ]] || maintenance_die "backup does not exist: ${backup}"
    [[ -x "${backup}/app/rp-console" ]] || maintenance_die "backup is missing the RP Console binary"
    [[ -f "${backup}/app/VERSION" ]] || maintenance_die "backup is missing the deployed version"
    [[ -f "${backup}/rp-console.service" ]] || maintenance_die "backup is missing the systemd unit"
    [[ -f "${backup}/rp-console.conf" ]] || maintenance_die "backup is missing the nginx site"

    systemctl stop rp-console.service >/dev/null 2>&1 || true
    rm -rf "${RP_CONSOLE_APP_DIR}" "${RP_CONSOLE_LIB_DIR}" "${RP_CONSOLE_ENV_DIR}" "${RP_CONSOLE_DATA_DIR}"
    rm -f "${RP_CONSOLE_UNIT_FILE}" "${RP_CONSOLE_NGINX_SITE}" "${RP_CONSOLE_NGINX_ENABLED}" "${RP_CONSOLE_SUDOERS}"

    cp -a "${backup}/app" "${RP_CONSOLE_APP_DIR}"
    if [[ -d "${backup}/lib" ]]; then
        cp -a "${backup}/lib" "${RP_CONSOLE_LIB_DIR}"
    fi
    cp -a "${backup}/etc" "${RP_CONSOLE_ENV_DIR}"
    mkdir -p -m 0700 "${RP_CONSOLE_DATA_DIR}"
    if [[ -d "${backup}/data" ]]; then
        cp -a "${backup}/data/." "${RP_CONSOLE_DATA_DIR}/"
    fi
    cp -a "${backup}/rp-console.service" "${RP_CONSOLE_UNIT_FILE}"
    cp -a "${backup}/rp-console.conf" "${RP_CONSOLE_NGINX_SITE}"
    if [[ -f "${backup}/rp-console-command" ]]; then
        cp -a "${backup}/rp-console-command" "${RP_CONSOLE_COMMAND}"
        chmod 0755 "${RP_CONSOLE_COMMAND}"
    fi
    if [[ -f "${backup}/rp-console-apply.sudoers" ]]; then
        cp -a "${backup}/rp-console-apply.sudoers" "${RP_CONSOLE_SUDOERS}"
        chmod 0440 "${RP_CONSOLE_SUDOERS}"
    fi
    if [[ -f "${backup}/nginx-enabled" ]]; then
        ln -s "${RP_CONSOLE_NGINX_SITE}" "${RP_CONSOLE_NGINX_ENABLED}"
    fi

    chown root:root "${RP_CONSOLE_APP_DIR}" "${RP_CONSOLE_APP_DIR}/rp-console" "${RP_CONSOLE_APP_DIR}/VERSION"
    chown -R rp-console:rp-console "${RP_CONSOLE_DATA_DIR}"
    chmod 0755 "${RP_CONSOLE_APP_DIR}" "${RP_CONSOLE_APP_DIR}/rp-console"
    chmod 0644 "${RP_CONSOLE_APP_DIR}/VERSION"
    chmod 0700 "${RP_CONSOLE_DATA_DIR}"
    chmod 0600 "${RP_CONSOLE_ENV_DIR}/rp-console.env"
    chmod 0600 "${RP_CONSOLE_ENV_DIR}/tls/origin.key"
    chmod 0644 "${RP_CONSOLE_ENV_DIR}/tls/origin.crt"
    systemctl daemon-reload
    maintenance_nginx_check
    systemctl reload nginx
    systemctl enable rp-console.service >/dev/null
    systemctl restart rp-console.service
    if ! maintenance_health_check "$(tr -d '\r\n' < "${RP_CONSOLE_APP_DIR}/VERSION")"; then
        maintenance_print_service_diagnostics
        return 1
    fi
}

maintenance_prune_backups() {
    maintenance_require_root
    local -a backups
    mapfile -t backups < <(find "${RP_CONSOLE_BACKUP_DIR}" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort -r)
    local index
    for ((index = 2; index < ${#backups[@]}; index++)); do
        rm -rf -- "${RP_CONSOLE_BACKUP_DIR}/${backups[index]}"
    done
}

maintenance_list_backups() {
    if [[ ! -d "${RP_CONSOLE_BACKUP_DIR}" ]]; then
        printf 'No RP Console backups are available.\n'
        return 0
    fi
    find "${RP_CONSOLE_BACKUP_DIR}" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort -r
}

maintenance_rollback() {
    maintenance_require_root
    local candidate current_snapshot target_version
    candidate="$(maintenance_list_backups | head -n 1)"
    [[ -n "${candidate}" ]] || maintenance_die "no backup is available for rollback"
    candidate="${RP_CONSOLE_BACKUP_DIR}/${candidate}"
    target_version="$(tr -d '\r\n' < "${candidate}/app/VERSION")"
    current_snapshot="$(maintenance_snapshot_current rollback-current)"

    if maintenance_restore_snapshot "${candidate}"; then
        maintenance_prune_backups
        printf 'RP Console restored to v%s.\n' "${target_version}"
        return 0
    fi

    printf 'rp-console: rollback failed; restoring the pre-rollback snapshot.\n' >&2
    maintenance_restore_snapshot "${current_snapshot}"
    maintenance_die "rollback failed; the previous current version was restored"
}

maintenance_main() {
    local action="${1:-}"
    case "${action}" in
        rollback)
            maintenance_rollback
            ;;
        backups)
            maintenance_list_backups
            ;;
        *)
            maintenance_die "unsupported maintenance action"
            ;;
    esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    maintenance_main "$@"
fi
