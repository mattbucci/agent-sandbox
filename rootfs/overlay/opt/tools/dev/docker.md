# docker

> Container runtime for building and running applications

**Category:** dev
**Binary:** /usr/bin/docker

## Quick Reference

```bash
# Build an image from Dockerfile
docker build -t myapp:latest .
# Run a container
docker run -d -p 8080:80 myapp:latest
# List running containers
docker ps
# View container logs
docker logs container-name
# Execute command in running container
docker exec -it container-name /bin/bash
```

## Examples

### Example: Build and Run
```bash
# Build image and start container
docker build -t myapp:latest .
docker run -d --name myapp -p 8080:80 myapp:latest
docker logs -f myapp
```

### Example: Run One-Off Command in Container
```bash
# Run a command and remove container after
docker run --rm -v $(pwd):/app -w /app python:3.11-slim python script.py
```

### Example: Inspect Container
```bash
# Check running containers and resource usage
docker ps
docker stats --no-stream
docker inspect container-name | jq '.[0].NetworkSettings.IPAddress'
```

### Example: Multi-Stage Build
```bash
# Build with specific target stage
docker build --target production -t myapp:prod .
```

### Example: Clean Up
```bash
# Remove stopped containers and unused images
docker system prune -f
```

## Key Flags
- `-t` — tag an image (name:version)
- `-d` — run container in detached mode
- `-p` — map host:container ports
- `-v` — bind mount volume (host:container)
- `-w` — set working directory in container
- `--rm` — auto-remove container on exit
- `-it` — interactive terminal
- `-f` — follow log output / specify Dockerfile
