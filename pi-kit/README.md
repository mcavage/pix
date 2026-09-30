# Pix sandbox kit

`pix/` is the Docker Sandbox Kit v3 workload. Build it with Docker Buildx:

```sh
docker buildx build pi-kit/pix -f pi-kit/pix/pix.yaml -t docker.io/mcavage/pix:VERSION --push
```

The workload layers the versioned `pix-agent` image with the native sandbox
network, credential, lifecycle, and agent-context capabilities. Local
development uses `make load`, which builds the kit under `out/kit/pix` against
the matching local agent image.
