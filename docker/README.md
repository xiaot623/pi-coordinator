# Docker Images

This directory defines the shared Docker layers used by the Docker runner.

Build the reusable base image:

```bash
docker build -f docker/Dockerfile --target base-node22 -t pico/pi-agent-base:node22 .
```

Build the standard pi agent image:

```bash
docker build -f docker/Dockerfile --target pi-agent -t pi-agent:latest .
```

Override the pi package if needed:

```bash
docker build -f docker/Dockerfile --target pi-agent \
  --build-arg PI_NPM_PACKAGE='@earendil-works/pi-coding-agent@0.78.1' \
  -t pi-agent:latest .
```

Personal layers should live outside this repository and inherit from `pi-agent:latest`.
For example:

```dockerfile
FROM pi-agent:latest

USER root
# Install personal language toolchains and tools here, such as Go or opencli.

USER pi
```

Use the image from pico config:

```yaml
runner:
  docker_image: "pi-agent:latest"
```
