# agent-browser

> Headless browser for web interaction via snapshot-based navigation

**Category:** browser
**Binary:** /usr/local/bin/agent-browser

## Quick Reference

```bash
# Navigate to a URL and get page snapshot
agent-browser navigate "https://example.com"
# Click an element by its snapshot ref
agent-browser click --ref 42
# Fill a form field
agent-browser fill --ref 15 --value "search query"
# Take a screenshot
agent-browser screenshot
# Get current page snapshot (DOM summary)
agent-browser snapshot
```

## Examples

### Example: Full Search Workflow
```bash
# 1. Navigate to the page
agent-browser navigate "https://github.com/search"
# 2. Get snapshot to find element refs
agent-browser snapshot
# 3. Fill the search box (use ref from snapshot)
agent-browser fill --ref 8 --value "python async patterns"
# 4. Click the search button
agent-browser click --ref 12
# 5. Get results snapshot
agent-browser snapshot
```

### Example: Reading Page Content
```bash
# Navigate and extract text content
agent-browser navigate "https://docs.python.org/3/library/asyncio.html"
agent-browser snapshot
```

### Example: Form Submission
```bash
# Fill multiple fields and submit
agent-browser navigate "https://example.com/login"
agent-browser snapshot
agent-browser fill --ref 5 --value "username"
agent-browser fill --ref 7 --value "password"
agent-browser click --ref 9
```

### Example: Following Links
```bash
# Click a link from the snapshot
agent-browser snapshot
# Identify the ref for the desired link from snapshot output
agent-browser click --ref 23
agent-browser snapshot
```

## Key Flags
- `--ref` — element reference number from snapshot output
- `--value` — text value for fill operations
- `--wait` — wait for page load after navigation
- `--timeout` — max wait time in milliseconds
