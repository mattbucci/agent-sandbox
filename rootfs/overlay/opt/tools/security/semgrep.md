# semgrep

> Static analysis tool for finding bugs and enforcing code patterns

**Category:** security
**Binary:** /usr/bin/semgrep

## Quick Reference

```bash
# Run default security rules on a project
semgrep --config auto .
# Run specific rule set
semgrep --config p/python .
# Scan a single file
semgrep --config auto src/main.py
# JSON output for parsing
semgrep --config auto --json .
# Run with specific severity
semgrep --config auto --severity ERROR .
```

## Examples

### Example: Security Audit of a Project
```bash
# Run all recommended security rules
semgrep --config auto --severity ERROR --severity WARNING .
```

### Example: Language-Specific Rules
```bash
# Python security patterns
semgrep --config p/python .
# JavaScript/TypeScript
semgrep --config p/javascript .
# OWASP Top 10
semgrep --config p/owasp-top-ten .
```

### Example: Scan and Output JSON
```bash
# Parse results programmatically
semgrep --config auto --json . | jq '.results[] | {file: .path, line: .start.line, rule: .check_id, message: .extra.message}'
```

### Example: Custom Pattern Search
```bash
# Find hardcoded secrets patterns
semgrep -e 'password = "..."' --lang python .
# Find eval usage
semgrep -e 'eval(...)' --lang python .
```

## Key Flags
- `--config` — rule configuration: auto, p/ruleset, or path to rules file
- `--json` — output in JSON format
- `--severity` — filter: ERROR, WARNING, INFO
- `-e` — inline pattern to search for
- `--lang` — language for inline patterns
- `--exclude` — glob patterns to exclude
- `--include` — glob patterns to include
