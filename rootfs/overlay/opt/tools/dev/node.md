# node

> Node.js JavaScript runtime

**Category:** dev
**Binary:** /usr/bin/node

## Quick Reference

```bash
# Run a script
node app.js
# Execute inline JavaScript
node -e "console.log(JSON.stringify({key: 'value'}, null, 2))"
# Run with environment variable
NODE_ENV=production node server.js
# Install dependencies
npm install
# Run package scripts
npm run build && npm test
```

## Examples

### Example: Quick JSON Processing
```bash
# Parse and transform JSON
cat data.json | node -e "
let chunks = [];
process.stdin.on('data', c => chunks.push(c));
process.stdin.on('end', () => {
  const data = JSON.parse(Buffer.concat(chunks));
  console.log(data.map(d => d.name).join('\n'));
});
"
```

### Example: Run a Script with Args
```bash
node build.js --env production --output dist/
```

### Example: Quick HTTP Server
```bash
# Serve static files on port 3000
npx serve -l 3000 ./dist
```

### Example: Evaluate Expression
```bash
# Quick calculations or string manipulation
node -e "console.log(Buffer.from('hello').toString('base64'))"
node -e "console.log(new Date().toISOString())"
```

## Key Flags
- `-e` — evaluate inline code
- `-p` — evaluate and print result
- `--max-old-space-size=4096` — increase memory limit (MB)
- `--experimental-modules` — enable ES module support
- `-r` — preload/require a module before executing
