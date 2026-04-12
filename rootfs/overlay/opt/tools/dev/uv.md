# uv

> Fast Python package manager and project tool (Rust-based pip/venv replacement)

**Category:** dev
**Binary:** /usr/bin/uv

## Quick Reference

```bash
# Create a virtual environment
uv venv
# Install packages (fast pip replacement)
uv pip install requests flask
# Install from requirements file
uv pip install -r requirements.txt
# Run a Python script with auto-managed deps
uv run script.py
# Initialize a new project
uv init my-project
```

## Examples

### Example: Set Up Project Environment
```bash
# Create venv and install deps
uv venv
source .venv/bin/activate
uv pip install -r requirements.txt
```

### Example: Add a Dependency
```bash
# Install and add to requirements
uv pip install httpx
uv pip freeze > requirements.txt
```

### Example: Run Script with Inline Dependencies
```bash
# Auto-install deps and run
uv run --with requests --with beautifulsoup4 scraper.py
```

### Example: Compile Locked Requirements
```bash
# Generate locked requirements from pyproject.toml
uv pip compile pyproject.toml -o requirements.lock
```

## Key Flags
- `venv` — create a virtual environment (.venv by default)
- `pip install` — install packages (drop-in pip replacement)
- `pip compile` — resolve and lock dependencies
- `run` — run a script with automatic dependency management
- `--with` — add inline dependency for `uv run`
- `--python` — specify Python version
