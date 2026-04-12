# jq

> Command-line JSON processor for parsing and transforming structured data

**Category:** data
**Binary:** /usr/bin/jq

## Quick Reference

```bash
# Pretty-print JSON
cat data.json | jq .
# Extract a field
cat data.json | jq '.name'
# Filter array elements
cat data.json | jq '.items[] | select(.status == "active")'
# Build new objects
cat data.json | jq '{name: .title, count: .items | length}'
# Raw string output (no quotes)
cat data.json | jq -r '.url'
```

## Examples

### Example: Extract Fields from API Response
```bash
# Get specific fields from GitHub API
curl -s https://api.github.com/repos/python/cpython | jq '{name, stars: .stargazers_count, language}'
```

### Example: Filter and Transform Arrays
```bash
# Find high-severity vulnerabilities
cat scan.json | jq '.results[] | .Vulnerabilities[] | select(.Severity == "CRITICAL") | {id: .VulnerabilityID, pkg: .PkgName}'
```

### Example: Aggregate Data
```bash
# Count items by category
cat data.json | jq 'group_by(.category) | map({category: .[0].category, count: length})'
```

### Example: Combine with curl
```bash
# Parse paginated API response
curl -s "https://api.example.com/items?page=1" | jq -r '.items[].name'
```

### Example: Modify JSON In-Place
```bash
# Update a field in a config file
jq '.version = "2.0.0"' package.json > tmp.json && mv tmp.json package.json
```

## Key Flags
- `-r` — raw output (no quotes around strings)
- `-e` — exit with error if output is null/false
- `-s` — slurp: read entire input as single array
- `-c` — compact output (one line)
- `--arg name value` — pass external variable into filter
- `-n` — null input (for generating JSON from scratch)
