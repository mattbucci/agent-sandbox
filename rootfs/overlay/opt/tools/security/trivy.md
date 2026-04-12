# trivy

> Comprehensive vulnerability scanner for containers, filesystems, and code

**Category:** security
**Binary:** /usr/bin/trivy

## Quick Reference

```bash
# Scan a container image
trivy image python:3.11-slim
# Scan the current filesystem/project
trivy fs .
# Scan for misconfigurations (IaC)
trivy config .
# Scan with JSON output for parsing
trivy image --format json -o results.json nginx:latest
# Scan only for critical/high vulnerabilities
trivy fs --severity CRITICAL,HIGH .
```

## Examples

### Example: Scan a Docker Image
```bash
# Full vulnerability scan of an image
trivy image --severity CRITICAL,HIGH python:3.11-slim
```

### Example: Scan Project Dependencies
```bash
# Scan project files for known vulnerabilities
trivy fs --scanners vuln .
```

### Example: Infrastructure as Code Scan
```bash
# Check Dockerfiles, Terraform, K8s manifests for misconfigs
trivy config --severity CRITICAL,HIGH .
```

### Example: JSON Output for Processing
```bash
# Output JSON and parse with jq
trivy fs --format json . | jq '.Results[].Vulnerabilities[] | {id: .VulnerabilityID, pkg: .PkgName, severity: .Severity}'
```

## Key Flags
- `--severity` — filter by severity: CRITICAL, HIGH, MEDIUM, LOW
- `--format` — output format: table, json, sarif
- `-o` — write output to file
- `--scanners` — choose scanners: vuln, misconfig, secret, license
- `--skip-dirs` — exclude directories from scan
- `--exit-code 1` — return non-zero if vulnerabilities found
