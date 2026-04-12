# pandoc

> Universal document converter between markup formats

**Category:** research
**Binary:** /usr/bin/pandoc

## Quick Reference

```bash
# Convert Markdown to HTML
pandoc input.md -o output.html
# Convert HTML to Markdown
pandoc input.html -t markdown -o output.md
# Convert Markdown to plain text
pandoc input.md -t plain
# Convert with template
pandoc input.md --template=template.html -o output.html
# Read from stdin
echo "# Hello" | pandoc -f markdown -t html
```

## Examples

### Example: HTML to Clean Markdown
```bash
# Convert downloaded HTML to readable markdown
curl -s https://example.com/docs | pandoc -f html -t markdown --wrap=none
```

### Example: Markdown to Plain Text
```bash
# Strip formatting for text analysis
pandoc README.md -t plain --wrap=none
```

### Example: Combine Multiple Files
```bash
# Merge markdown files into one document
pandoc chapter1.md chapter2.md chapter3.md -o book.html
```

### Example: Convert Between Markup Formats
```bash
# RST to Markdown
pandoc docs.rst -f rst -t markdown -o docs.md
# LaTeX to Markdown
pandoc paper.tex -f latex -t markdown -o paper.md
```

### Example: Extract Text from HTML Page
```bash
# Fetch and convert to plain text
curl -s https://example.com/article | pandoc -f html -t plain --wrap=none
```

## Key Flags
- `-f` — input format (markdown, html, rst, latex, docx, etc.)
- `-t` — output format
- `-o` — output file path
- `--wrap=none` — disable line wrapping
- `--standalone` — produce complete document with header/footer
- `--template` — use custom template
- `--extract-media` — extract images to directory
