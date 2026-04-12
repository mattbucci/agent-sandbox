# grype

> Vulnerability scanner for container images and filesystems

**Category:** security
**Binary:** /usr/bin/grype

## Quick Reference

```bash
# Scan a container image
grype python:3.11-slim
# Scan a local directory
grype dir:.
# Scan an SBOM file
grype sbom:./sbom.json
# Filter by severity
grype --only-fixed --fail-on critical python:3.11-slim
# JSON output
grype -o json python:3.11-slim
```

## Examples

### Example: Scan Docker Image
```bash
# Find vulnerabilities in an image
grype python:3.11-slim --only-fixed
```

### Example: Scan Project Directory
```bash
# Scan local project dependencies
grype dir:. -o table
```

### Example: Filter Critical Vulnerabilities
```bash
# Show only critical/high and fail if found
grype --fail-on high python:3.11-slim
```

### Example: JSON Output for Processing
```bash
# Parse results with jq
grype -o json dir:. | jq '.matches[] | {pkg: .artifact.name, version: .artifact.version, vuln: .vulnerability.id, severity: .vulnerability.severity}'
```

## Key Flags
- `-o` — output format: table, json, cyclonedx, sarif
- `--fail-on` — return non-zero for severity: critical, high, medium, low
- `--only-fixed` — show only vulnerabilities with available fixes
- `--scope` — scan scope: squashed (default) or all-layers
- `--add-cpes-if-none` — generate CPEs for packages missing them
