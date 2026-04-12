# grep

> Search file contents for patterns using regular expressions

**Category:** system
**Binary:** /usr/bin/grep

## Quick Reference

```bash
# Search for a pattern in files
grep -r "TODO" src/
# Case-insensitive search
grep -ri "error" logs/
# Show line numbers and context
grep -rn -C 3 "def main" *.py
# Search for exact word
grep -rw "config" .
# Invert match (lines NOT matching)
grep -v "DEBUG" app.log
```

## Examples

### Example: Search Codebase for Pattern
```bash
# Find all function definitions
grep -rn "def " --include="*.py" src/
```

### Example: Find Configuration Values
```bash
# Search for environment variable usage
grep -rn "os.environ\|os.getenv" --include="*.py" .
```

### Example: Filter Log Output
```bash
# Show only error lines with context
grep -C 2 "ERROR\|CRITICAL" /var/log/app.log
```

### Example: Count Matches
```bash
# Count occurrences per file
grep -rc "import" --include="*.py" src/
```

### Example: Search with Regex
```bash
# Find IP addresses
grep -rE "\b[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\b" config/
# Find email patterns
grep -rE "[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}" .
```

## Key Flags
- `-r` — recursive search through directories
- `-n` — show line numbers
- `-i` — case-insensitive matching
- `-w` — match whole words only
- `-C N` — show N lines of context around matches
- `-l` — list only filenames with matches
- `-c` — count matches per file
- `-v` — invert match (show non-matching lines)
- `-E` — extended regex (egrep)
- `--include` — filter files by glob pattern
- `--exclude-dir` — skip directories
