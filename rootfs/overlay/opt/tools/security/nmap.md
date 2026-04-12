# nmap

> Network scanner for port discovery and service detection

**Category:** security
**Binary:** /usr/bin/nmap

## Quick Reference

```bash
# Quick scan of common ports
nmap 192.168.1.1
# Scan specific ports
nmap -p 80,443,8080 target.com
# Service version detection
nmap -sV target.com
# Scan a subnet
nmap 192.168.1.0/24
# Fast scan (top 100 ports)
nmap -F target.com
```

## Examples

### Example: Service Discovery
```bash
# Detect services and versions on open ports
nmap -sV -p 1-1000 target.com
```

### Example: Check Specific Ports
```bash
# Verify if web services are running
nmap -p 80,443,8080,8443 target.com
```

### Example: Scan with OS Detection
```bash
# Identify operating system and services
nmap -sV -O target.com
```

### Example: Output to Parseable Format
```bash
# XML output for processing
nmap -sV -oX scan_results.xml target.com
# Grep-friendly output
nmap -sV -oG scan_results.txt target.com
```

### Example: Scan Local Network
```bash
# Discover hosts on a subnet
nmap -sn 192.168.1.0/24
```

## Key Flags
- `-p` — specify ports (e.g., -p 80,443 or -p 1-1000)
- `-sV` — probe open ports for service/version info
- `-sn` — ping scan only, no port scan
- `-F` — fast scan (top 100 ports)
- `-O` — enable OS detection
- `-oX` — XML output
- `-oG` — grepable output
- `--open` — show only open ports
