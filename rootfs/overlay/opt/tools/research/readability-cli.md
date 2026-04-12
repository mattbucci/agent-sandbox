# readability-cli

> Extract readable article content from web pages (Mozilla Readability)

**Category:** research
**Binary:** /usr/local/bin/readable

## Quick Reference

```bash
# Extract article text from a URL
readable https://example.com/article
# Output as plain text
readable --low-confidence force -p text https://example.com/article
# Output as Markdown
readable -p markdown https://example.com/article
# Extract from local HTML file
readable local-page.html
# Show metadata (title, author, excerpt)
readable --properties title,byline,excerpt https://example.com/article
```

## Examples

### Example: Extract Article Content
```bash
# Get clean readable text from a web article
readable -p text https://blog.example.com/post-title
```

### Example: Save as Markdown
```bash
# Download and convert to markdown for analysis
readable -p markdown https://docs.example.com/guide > guide.md
```

### Example: Get Article Metadata
```bash
# Extract title and summary
readable --properties title,excerpt,byline https://example.com/article
```

### Example: Process Local HTML
```bash
# Extract from saved HTML
curl -s -o page.html https://example.com/article
readable -p text page.html
```

## Key Flags
- `-p` — output property: text, html, markdown, title, excerpt
- `--properties` — comma-separated list of metadata fields
- `--low-confidence force` — extract even if confidence is low
- `--base` — set base URL for resolving relative links
- `--url` — specify source URL when reading from stdin
