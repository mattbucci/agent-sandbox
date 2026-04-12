# pip-audit

> Audit Python dependencies for known vulnerabilities

**Category:** security
**Binary:** /usr/bin/pip-audit

## Quick Reference

```bash
# Audit current environment
pip-audit
# Audit a requirements file
pip-audit -r requirements.txt
# JSON output
pip-audit --format json
# Fix vulnerabilities automatically
pip-audit --fix
# Audit with specific vulnerability source
pip-audit --vulnerability-service osv
```

## Examples

### Example: Audit Project Requirements
```bash
# Check requirements.txt for known vulns
pip-audit -r requirements.txt
```

### Example: Audit and Auto-Fix
```bash
# Attempt to upgrade vulnerable packages
pip-audit -r requirements.txt --fix
```

### Example: JSON Output for Processing
```bash
# Parse results
pip-audit --format json | jq '.dependencies[] | select(.vulns | length > 0) | {name, version, vulns: [.vulns[].id]}'
```

### Example: Strict Audit with Exit Code
```bash
# Fail CI if any vulnerabilities found
pip-audit -r requirements.txt --strict
```

## Key Flags
- `-r` — path to requirements file
- `--format` — output format: columns, json, cyclonedx-json, markdown
- `--fix` — attempt to fix vulnerable packages
- `--strict` — fail on any warnings
- `--vulnerability-service` — source: pypi (default) or osv
- `--desc` — include vulnerability descriptions
