# awk

> Pattern-based text processing and data extraction

**Category:** system
**Binary:** /usr/bin/awk

## Quick Reference

```bash
# Print specific columns
awk '{print $1, $3}' file.txt
# Filter rows by condition
awk '$3 > 100' data.txt
# Custom field separator
awk -F',' '{print $2}' data.csv
# Sum a column
awk '{sum += $2} END {print sum}' data.txt
# Match pattern and print
awk '/ERROR/ {print $0}' log.txt
```

## Examples

### Example: Parse CSV Data
```bash
# Extract name and email columns from CSV
awk -F',' '{print $1, $3}' users.csv
```

### Example: Process Log Files
```bash
# Extract timestamps and error messages
awk '/ERROR/ {print $1, $2, substr($0, index($0,$5))}' app.log
```

### Example: Summarize Data
```bash
# Count unique values in a column
awk -F',' '{count[$2]++} END {for (k in count) print k, count[k]}' data.csv
```

### Example: Transform Delimited Data
```bash
# Convert CSV to TSV
awk -F',' '{OFS="\t"; $1=$1; print}' data.csv
```

### Example: Filter by Column Value
```bash
# Show processes using more than 1GB memory
ps aux | awk '$6 > 1048576 {print $11, $6/1024 "MB"}'
```

## Key Flags
- `-F` — set field separator (default: whitespace)
- `-v var=value` — set variable before execution
- `NR` — current record/line number
- `NF` — number of fields in current line
- `$0` — entire current line
- `$N` — Nth field
- `BEGIN{}` — execute before processing
- `END{}` — execute after all input processed
