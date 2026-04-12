# gh

> GitHub CLI for issues, PRs, and repository operations

**Category:** git
**Binary:** /usr/bin/gh

## Quick Reference

```bash
# View an issue
gh issue view 42
# Create a pull request
gh pr create --title "Fix bug" --body "Description"
# List open PRs
gh pr list
# View PR details and diff
gh pr view 123 && gh pr diff 123
# Read PR review comments
gh api repos/owner/repo/pulls/123/comments
```

## Examples

### Example: Read an Issue
```bash
# View issue details including comments
gh issue view 42 --repo owner/repo
gh issue view 42 --comments
```

### Example: Create a Pull Request
```bash
gh pr create --title "Fix request timeout" --body "$(cat <<'EOF'
## Summary
- Fixed timeout handling in HTTP client
- Added retry logic for transient failures

## Test plan
- [ ] Unit tests pass
- [ ] Manual test with slow endpoint
EOF
)"
```

### Example: Review PR Status and Checks
```bash
# View PR status
gh pr view 123
# View CI check status
gh pr checks 123
# View the diff
gh pr diff 123
```

### Example: Search Issues
```bash
# Find open bugs with a label
gh issue list --label "bug" --state open --repo owner/repo
# Search across repos
gh search issues "memory leak" --language python
```

### Example: Read PR Comments via API
```bash
gh api repos/owner/repo/pulls/123/comments --jq '.[].body'
```

## Key Flags
- `--repo owner/repo` — target a specific repository
- `--title` — PR or issue title
- `--body` — PR or issue description
- `--comments` — include comments in issue/PR view
- `--jq` — filter JSON API responses
- `--label` — filter by label
- `--state` — filter by open/closed/all
