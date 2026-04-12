# find

> Search for files and directories by name, type, size, or modification time

**Category:** system
**Binary:** /usr/bin/find

## Quick Reference

```bash
# Find files by name
find . -name "*.py" -type f
# Find recently modified files
find . -mtime -1 -type f
# Find large files
find . -size +10M -type f
# Find and execute command on results
find . -name "*.log" -exec rm {} \;
# Find excluding directories
find . -name "*.js" -not -path "*/node_modules/*"
```

## Examples

### Example: Find Source Files
```bash
# Find all Python files, excluding venv
find /project -name "*.py" -type f -not -path "*/.venv/*" -not -path "*/__pycache__/*"
```

### Example: Find Recently Changed Files
```bash
# Files modified in the last hour
find . -mmin -60 -type f
# Files modified in the last day
find . -mtime -1 -type f -ls
```

### Example: Find and Count by Extension
```bash
# Count files by type
find . -type f -name "*.py" | wc -l
```

### Example: Find Empty or Large Files
```bash
# Find empty files
find . -type f -empty
# Find files over 100MB
find . -type f -size +100M -exec ls -lh {} \;
```

### Example: Find with Multiple Conditions
```bash
# Find config files modified recently
find /etc -type f \( -name "*.conf" -o -name "*.cfg" \) -mtime -7
```

## Key Flags
- `-name` — match filename pattern (case-sensitive)
- `-iname` — match filename pattern (case-insensitive)
- `-type f/d` — file or directory
- `-mtime -N` — modified in last N days
- `-mmin -N` — modified in last N minutes
- `-size +N` — larger than N (k, M, G suffixes)
- `-exec` — run command on each match
- `-not -path` — exclude paths matching pattern
