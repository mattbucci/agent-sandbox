# python3

> Python 3 interpreter for scripting and development

**Category:** dev
**Binary:** /usr/bin/python3

## Quick Reference

```bash
# Run a Python script
python3 script.py
# Execute inline code
python3 -c "import json; print(json.dumps({'key': 'value'}, indent=2))"
# Run a module
python3 -m http.server 8080
# Check syntax without executing
python3 -m py_compile script.py
# Install package in current environment
python3 -m pip install requests
```

## Examples

### Example: Quick Data Processing
```bash
# Parse and transform JSON from stdin
cat data.json | python3 -c "
import json, sys
data = json.load(sys.stdin)
for item in data:
    print(f\"{item['name']}: {item['value']}\")
"
```

### Example: Run a Script with Arguments
```bash
python3 analyze.py --input data.csv --output results.json
```

### Example: One-Liner for File Processing
```bash
# Count lines matching a pattern
python3 -c "
import re
with open('log.txt') as f:
    errors = [l for l in f if re.search(r'ERROR|CRITICAL', l)]
print(f'{len(errors)} errors found')
"
```

### Example: Quick HTTP Request
```bash
python3 -c "
import urllib.request, json
resp = urllib.request.urlopen('https://api.github.com/repos/python/cpython')
data = json.loads(resp.read())
print(f\"Stars: {data['stargazers_count']}\")
"
```

## Key Flags
- `-c` — execute inline code string
- `-m` — run a module as a script
- `-B` — don't write .pyc bytecode files
- `-u` — unbuffered stdout/stderr
- `-W ignore` — suppress warnings
