#!/usr/bin/env bash
# =============================================================================
# lib/versions.sh — load the pinned versions and fetch artifacts verified.
#
# Sourced by host scripts. The same fetch_verified function is prepended to the
# compiled customize.sh (lib/agentconf.py) so install scripts inside the rootfs
# chroot use it too — keep this file free of host-only dependencies.
# =============================================================================

# Load config/versions.env when running from a checkout. Inside the chroot the
# values are already inlined above this function, so the file is absent there.
if [[ -n "${SANDBOX_ROOT:-}" && -f "${SANDBOX_ROOT}/config/versions.env" ]]; then
    # shellcheck source=../config/versions.env
    source "${SANDBOX_ROOT}/config/versions.env"
fi

# fetch_verified <url> <sha256> <output-path>
#
# Download to a temp file, compare the SHA-256, and only then move it into
# place — a mismatching artifact never lands at <output-path>. An empty
# checksum is refused unless ALLOW_UNVERIFIED=1 (non-x86_64 architectures,
# where versions.env carries no checksum).
fetch_verified() {
    local url="$1" want="$2" out="$3"
    local tmp got
    tmp="$(mktemp "${out}.XXXXXX")"

    if ! curl -fsSL --retry 3 --retry-delay 2 -o "${tmp}" "${url}"; then
        rm -f "${tmp}"
        echo "fetch_verified: download failed: ${url}" >&2
        return 1
    fi

    if [[ -z "${want}" ]]; then
        if [[ "${ALLOW_UNVERIFIED:-0}" != "1" ]]; then
            rm -f "${tmp}"
            echo "fetch_verified: no checksum pinned for ${url}" >&2
            echo "  (set ALLOW_UNVERIFIED=1 to install without verification)" >&2
            return 1
        fi
        echo "fetch_verified: WARNING: installing UNVERIFIED ${url}" >&2
    else
        got="$(sha256sum "${tmp}" | awk '{print $1}')"
        if [[ "${got}" != "${want}" ]]; then
            rm -f "${tmp}"
            echo "fetch_verified: CHECKSUM MISMATCH for ${url}" >&2
            echo "  expected ${want}" >&2
            echo "  got      ${got}" >&2
            return 1
        fi
    fi

    mv -f "${tmp}" "${out}"
}

# pinned_sha <sha256> — echo the checksum on x86_64, nothing elsewhere (the
# manifest only carries x86_64 checksums).
pinned_sha() {
    case "$(uname -m)" in
        x86_64|amd64) echo "$1" ;;
        *)            echo "" ;;
    esac
}
