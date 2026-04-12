# git

> Distributed version control system

**Category:** git
**Binary:** /usr/bin/git

## Quick Reference

```bash
# Clone a repository
git clone https://github.com/owner/repo.git
# Create and switch to a new branch
git checkout -b feature/my-change
# Stage, commit, and push
git add file.py && git commit -m "Add feature" && git push -u origin feature/my-change
# View status and diff
git status && git diff
# View recent commit history
git log --oneline -10
```

## Examples

### Example: Clone and Branch Workflow
```bash
# Clone and prepare a feature branch
git clone https://github.com/owner/repo.git
cd repo
git checkout -b fix/issue-42
```

### Example: Stage and Commit Changes
```bash
# Stage specific files (preferred over git add .)
git add src/main.py tests/test_main.py
git commit -m "Fix null pointer in request handler"
```

### Example: Inspect Changes Before Committing
```bash
# See what changed
git diff
# See what's staged
git diff --cached
# See untracked files
git status
```

### Example: Push and Set Upstream
```bash
git push -u origin feature/my-change
```

### Example: View File at a Specific Commit
```bash
git show HEAD~3:src/config.py
```

## Key Flags
- `--oneline` — compact log output, one line per commit
- `-b` — create a new branch (with checkout)
- `-u origin` — set upstream tracking branch (with push)
- `--cached` — show staged changes (with diff)
- `-m` — inline commit message
