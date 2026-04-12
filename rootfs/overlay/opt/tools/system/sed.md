# sed

> Stream editor for text transformation and substitution

**Category:** system
**Binary:** /usr/bin/sed

## Quick Reference

```bash
# Replace first occurrence per line
sed 's/old/new/' file.txt
# Replace all occurrences
sed 's/old/new/g' file.txt
# Edit file in place
sed -i 's/old/new/g' file.txt
# Delete lines matching pattern
sed '/pattern/d' file.txt
# Print specific line range
sed -n '10,20p' file.txt
```

## Examples

### Example: Find and Replace in Files
```bash
# Update version string across a project
sed -i 's/version = "1.0.0"/version = "1.1.0"/g' setup.py
```

### Example: Remove Lines
```bash
# Remove comment lines
sed '/^#/d' config.txt
# Remove blank lines
sed '/^$/d' file.txt
```

### Example: Extract Lines by Range
```bash
# Print lines 50-75
sed -n '50,75p' large_file.txt
# Print from pattern to end of file
sed -n '/START_MARKER/,$p' log.txt
```

### Example: Insert or Append Text
```bash
# Add a line after a match
sed '/\[dependencies\]/a new_package = "1.0"' Cargo.toml
# Add a line before a match
sed '/\[dependencies\]/i # Auto-generated dependencies' Cargo.toml
```

### Example: Multiple Substitutions
```bash
# Chain multiple edits
sed -e 's/foo/bar/g' -e 's/baz/qux/g' file.txt
```

## Key Flags
- `-i` — edit file in place (modifies original)
- `-n` — suppress auto-print (use with p command)
- `-e` — add multiple editing commands
- `-E` — extended regex support
- `s/old/new/g` — substitute globally
- `/pattern/d` — delete matching lines
- `Np` — print line N
