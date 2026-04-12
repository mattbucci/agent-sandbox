#!/usr/bin/env bash
# =============================================================================
# setup-github-tokens.sh — Create and validate GitHub fine-grained tokens
#
# Creates per-agent GitHub fine-grained personal access tokens with the
# minimum required permissions. Validates that tokens are fine-grained
# (not classic) and rejects SSH keys.
#
# Usage:
#   bin/setup-github-tokens.sh                    # interactive setup
#   bin/setup-github-tokens.sh validate           # validate existing tokens
#   bin/setup-github-tokens.sh validate <type>    # validate one agent's token
#   bin/setup-github-tokens.sh show               # show what each agent needs
#
# Token storage: config/secrets/github-tokens/<agent-type>.token
# These files are gitignored and injected into VMs at boot.
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${SANDBOX_ROOT}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'

TOKENS_DIR="${SANDBOX_ROOT}/config/secrets/github-tokens"
mkdir -p "${TOKENS_DIR}"
chmod 700 "${SANDBOX_ROOT}/config/secrets" 2>/dev/null || true
chmod 700 "${TOKENS_DIR}" 2>/dev/null || true

# Agent token requirements — defines the minimum permissions per agent type
declare -A AGENT_PERMISSIONS
AGENT_PERMISSIONS=(
    [debugger]="contents:read, issues:read, pull_requests:read"
    [feature-dev]="contents:write, issues:read, pull_requests:write, workflows:read"
    [devops]="contents:read, actions:write, deployments:write, environments:read"
    [researcher]=""  # no token needed — public API only
    [security]="contents:read, security_events:read, dependabot_alerts:read"
)

declare -A AGENT_DESCRIPTIONS
AGENT_DESCRIPTIONS=(
    [debugger]="Read code and issues to investigate bugs. No write access needed."
    [feature-dev]="Read issues, write code, create PRs. Needs push + PR permissions."
    [devops]="Trigger deployments and manage actions. Read code, write actions."
    [researcher]="No GitHub token needed. Uses public API for trend monitoring."
    [security]="Read code and security advisories. No write access needed."
)

# =========================================================================
# Token validation
# =========================================================================

validate_token() {
    local agent_type="$1"
    local token_file="${TOKENS_DIR}/${agent_type}.token"
    local errors=0

    if [[ ! -f "${token_file}" ]]; then
        if [[ "${agent_type}" == "researcher" ]]; then
            echo -e "  ${GREEN}[OK]${NC} ${agent_type}: no token (not required)"
            return 0
        fi
        echo -e "  ${YELLOW}[MISSING]${NC} ${agent_type}: no token at ${token_file}"
        return 1
    fi

    local token
    token=$(cat "${token_file}" | tr -d '[:space:]')

    # Check for empty token
    if [[ -z "${token}" ]]; then
        echo -e "  ${RED}[ERROR]${NC} ${agent_type}: token file is empty"
        return 1
    fi

    # SECURITY: Reject classic personal access tokens (ghp_)
    if [[ "${token}" == ghp_* ]]; then
        echo -e "  ${RED}[SECURITY RISK]${NC} ${agent_type}: CLASSIC token detected (ghp_*)"
        echo -e "    Classic tokens have broad, unscoped access to ALL your repos."
        echo -e "    They cannot be restricted to specific repositories."
        echo -e "    ${YELLOW}Replace with a fine-grained token (github_pat_*).${NC}"
        echo -e "    See: https://github.com/settings/personal-access-tokens/new"
        return 1
    fi

    # SECURITY: Reject GitHub OAuth tokens (gho_)
    if [[ "${token}" == gho_* ]]; then
        echo -e "  ${RED}[SECURITY RISK]${NC} ${agent_type}: OAuth token detected (gho_*)"
        echo -e "    OAuth tokens are app-scoped and may have broad permissions."
        echo -e "    ${YELLOW}Replace with a fine-grained token (github_pat_*).${NC}"
        return 1
    fi

    # SECURITY: Reject SSH keys
    if [[ "${token}" == ssh-* ]] || [[ "${token}" == ecdsa-* ]] || [[ "${token}" == "-----BEGIN"* ]]; then
        echo -e "  ${RED}[SECURITY RISK]${NC} ${agent_type}: SSH KEY detected"
        echo -e "    SSH keys grant full push access to ALL repos you have access to."
        echo -e "    They cannot be scoped to specific repos or permissions."
        echo -e "    ${YELLOW}Never give agents SSH keys. Use fine-grained tokens instead.${NC}"
        return 1
    fi

    # SECURITY: Reject GitHub App installation tokens (ghs_)
    if [[ "${token}" == ghs_* ]]; then
        echo -e "  ${RED}[SECURITY RISK]${NC} ${agent_type}: GitHub App installation token (ghs_*)"
        echo -e "    These are short-lived but may have broad org-level permissions."
        echo -e "    ${YELLOW}Replace with a fine-grained personal access token.${NC}"
        return 1
    fi

    # Validate it IS a fine-grained token (github_pat_)
    if [[ "${token}" != github_pat_* ]]; then
        echo -e "  ${RED}[ERROR]${NC} ${agent_type}: unrecognized token format"
        echo -e "    Expected: github_pat_* (fine-grained personal access token)"
        echo -e "    Got: ${token:0:10}..."
        return 1
    fi

    # Check file permissions
    local perms
    perms=$(stat -c '%a' "${token_file}" 2>/dev/null || stat -f '%Lp' "${token_file}" 2>/dev/null)
    if [[ "${perms}" != "600" ]] && [[ "${perms}" != "400" ]]; then
        echo -e "  ${YELLOW}[WARN]${NC} ${agent_type}: token file permissions are ${perms} (should be 600)"
        chmod 600 "${token_file}"
        echo -e "    Fixed to 600."
    fi

    # Test the token against the GitHub API
    echo -ne "  Validating ${agent_type} token... "
    local api_result
    api_result=$(curl -s -w "\n%{http_code}" -H "Authorization: Bearer ${token}" \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/user" 2>/dev/null)
    local http_code
    http_code=$(echo "${api_result}" | tail -1)
    local body
    body=$(echo "${api_result}" | head -n -1)

    if [[ "${http_code}" == "200" ]]; then
        local username
        username=$(echo "${body}" | python3 -c "import sys,json; print(json.load(sys.stdin).get('login','?'))" 2>/dev/null)
        echo -e "${GREEN}valid${NC} (user: ${username})"

        # Check token expiration
        local expires
        expires=$(curl -s -H "Authorization: Bearer ${token}" \
            -H "Accept: application/vnd.github+json" \
            "https://api.github.com/user" -D - 2>/dev/null | grep -i "github-authentication-token-expiration" | cut -d: -f2- | tr -d '[:space:]')
        if [[ -n "${expires}" ]]; then
            echo -e "    Expires: ${expires}"
        fi
    elif [[ "${http_code}" == "401" ]]; then
        echo -e "${RED}invalid (401 unauthorized)${NC}"
        errors=1
    elif [[ "${http_code}" == "403" ]]; then
        echo -e "${YELLOW}rate limited or IP restricted (403)${NC}"
    else
        echo -e "${YELLOW}could not verify (HTTP ${http_code})${NC}"
    fi

    if [[ ${errors} -eq 0 ]]; then
        echo -e "  ${GREEN}[OK]${NC} ${agent_type}: fine-grained token"
    fi
    return ${errors}
}

# =========================================================================
# Commands
# =========================================================================

cmd_show() {
    echo "=========================================="
    echo "  GitHub Token Requirements per Agent"
    echo "=========================================="
    echo ""

    for agent_type in debugger feature-dev devops researcher security; do
        local perms="${AGENT_PERMISSIONS[$agent_type]}"
        local desc="${AGENT_DESCRIPTIONS[$agent_type]}"
        local token_file="${TOKENS_DIR}/${agent_type}.token"
        local status="MISSING"
        [[ -f "${token_file}" ]] && status="CONFIGURED"
        [[ "${agent_type}" == "researcher" ]] && [[ ! -f "${token_file}" ]] && status="NOT NEEDED"

        echo -e "  ${CYAN}${agent_type}${NC} [${status}]"
        echo "    ${desc}"
        if [[ -n "${perms}" ]]; then
            echo "    Permissions: ${perms}"
        else
            echo "    Permissions: none (public API access only)"
        fi
        echo "    Token file: ${token_file}"
        echo ""
    done

    echo "To create tokens, go to:"
    echo "  https://github.com/settings/personal-access-tokens/new"
    echo ""
    echo "Requirements:"
    echo "  - Token type: Fine-grained personal access token"
    echo "  - Resource owner: your-org-or-username"
    echo "  - Repository access: Only select repositories (pick specific repos)"
    echo "  - Expiration: 90 days recommended (set a calendar reminder)"
    echo ""
    echo "NEVER use:"
    echo "  - Classic tokens (ghp_*) — unscoped, access to ALL repos"
    echo "  - SSH keys — unscoped, full push to ALL repos"
    echo "  - OAuth tokens (gho_*) — may have broad org permissions"
}

cmd_validate() {
    local agent_type="${1:-}"
    local errors=0

    echo "=========================================="
    echo "  GitHub Token Validation"
    echo "=========================================="
    echo ""

    if [[ -n "${agent_type}" ]]; then
        validate_token "${agent_type}" || errors=1
    else
        for agent in debugger feature-dev devops researcher security; do
            validate_token "${agent}" || errors=$((errors + 1))
        done
    fi

    echo ""
    if [[ ${errors} -gt 0 ]]; then
        echo -e "${RED}${errors} token issue(s) found.${NC}"
        echo "Run 'bin/setup-github-tokens.sh show' for setup instructions."
        return 1
    else
        echo -e "${GREEN}All tokens validated.${NC}"
    fi
}

cmd_setup_interactive() {
    echo "=========================================="
    echo "  GitHub Token Setup (Interactive)"
    echo "=========================================="
    echo ""
    echo "This will guide you through creating fine-grained tokens for each agent."
    echo ""

    for agent_type in debugger feature-dev devops researcher security; do
        local perms="${AGENT_PERMISSIONS[$agent_type]}"
        local desc="${AGENT_DESCRIPTIONS[$agent_type]}"
        local token_file="${TOKENS_DIR}/${agent_type}.token"

        echo -e "--- ${CYAN}${agent_type}${NC} ---"
        echo "  ${desc}"

        if [[ "${agent_type}" == "researcher" ]]; then
            echo "  No token needed. Skipping."
            echo ""
            continue
        fi

        if [[ -f "${token_file}" ]]; then
            echo -e "  Token already exists at ${token_file}"
            read -rp "  Replace? [y/N] " replace
            if [[ "${replace}" != "y" && "${replace}" != "Y" ]]; then
                echo ""
                continue
            fi
        fi

        echo ""
        echo "  Create a fine-grained token at:"
        echo "    https://github.com/settings/personal-access-tokens/new"
        echo ""
        echo "  Settings:"
        echo "    Token name: agent-sandbox-${agent_type}"
        echo "    Expiration: 90 days"
        echo "    Repository access: Only select repositories"
        echo "    Permissions: ${perms}"
        echo ""
        read -rp "  Paste the token (github_pat_...): " token

        if [[ -z "${token}" ]]; then
            echo "  Skipped."
            echo ""
            continue
        fi

        # Write and validate
        echo "${token}" > "${token_file}"
        chmod 600 "${token_file}"
        echo ""
        validate_token "${agent_type}"
        echo ""
    done
}

cmd_help() {
    cat <<'HELP'
setup-github-tokens.sh — Manage GitHub tokens for agent sandboxes

Usage:
  bin/setup-github-tokens.sh              Interactive token setup
  bin/setup-github-tokens.sh show         Show requirements per agent
  bin/setup-github-tokens.sh validate     Validate all configured tokens
  bin/setup-github-tokens.sh validate X   Validate one agent's token
  bin/setup-github-tokens.sh help         This help message

Token storage:
  config/secrets/github-tokens/<agent>.token

Security rules enforced:
  - ONLY fine-grained tokens accepted (github_pat_*)
  - Classic tokens REJECTED (ghp_*) — too broad
  - SSH keys REJECTED — unscoped, full repo access
  - OAuth tokens REJECTED (gho_*) — may have broad permissions
  - GitHub App tokens REJECTED (ghs_*) — org-level access
  - Token files must be mode 600
HELP
}

# =========================================================================
# Main
# =========================================================================

case "${1:-}" in
    show)          cmd_show ;;
    validate)      shift; cmd_validate "$@" ;;
    help|--help)   cmd_help ;;
    *)             cmd_setup_interactive ;;
esac
