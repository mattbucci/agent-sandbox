# curl

> Command-line HTTP client for API requests and file downloads

**Category:** data
**Binary:** /usr/bin/curl

## Quick Reference

```bash
# GET request
curl -s https://api.example.com/data
# POST JSON data
curl -s -X POST -H "Content-Type: application/json" -d '{"key":"value"}' https://api.example.com/data
# Download a file
curl -L -o output.tar.gz https://example.com/file.tar.gz
# With authentication header
curl -s -H "Authorization: Bearer $TOKEN" https://api.example.com/me
# Follow redirects and show headers
curl -sL -D - https://example.com
```

## Examples

### Example: API GET with JSON Parsing
```bash
# Fetch and parse API response
curl -s https://api.github.com/repos/python/cpython | jq '{name, stars: .stargazers_count}'
```

### Example: POST JSON Payload
```bash
curl -s -X POST \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d '{"prompt": "Hello", "max_tokens": 100}' \
  https://api.example.com/generate
```

### Example: Download File with Progress
```bash
# Download and follow redirects
curl -L -o installer.sh https://example.com/install.sh
chmod +x installer.sh
```

### Example: Check HTTP Status Code
```bash
# Get only the status code
curl -s -o /dev/null -w "%{http_code}" https://example.com
```

### Example: Upload a File
```bash
curl -X POST -F "file=@report.pdf" https://api.example.com/upload
```

## Key Flags
- `-s` — silent mode (no progress bar)
- `-L` — follow redirects
- `-o` — write output to file
- `-X` — HTTP method (GET, POST, PUT, DELETE)
- `-H` — add header
- `-d` — request body data
- `-F` — multipart form data / file upload
- `-w` — custom output format string
- `-k` — allow insecure SSL connections
