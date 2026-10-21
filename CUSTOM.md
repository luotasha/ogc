# Custom OGC build

This branch adds the local features used with Claude Code and OpenCode Go:

- multiple named accounts with session-sticky key routing;
- per-account default models and per-session model selection through `automodel`;
- localhost administration UI at `http://127.0.0.1:3456/admin/`;
- durable session bindings, last-communication time, and local conversation previews;
- upstream error preservation and safer circuit-breaker classification;
- stable Claude session propagation;
- request ordering and required `content` field compatibility fixes;
- a reasoning compatibility proxy that restores `reasoning_content` for tool-call histories.

## Build

```bash
make test
make build
```

This creates:

```text
bin/ogc
bin/ogc-reasoning-proxy
```

Cross-platform artifacts for macOS, Linux, and Windows are built by:

```bash
make dist
```

GitHub Actions runs tests and builds both executables on every branch push and
pull request. Pushing a `v*` tag also creates a GitHub Release containing all
binaries and checksums.

## Run

Start the reasoning compatibility proxy first:

```bash
./bin/ogc-reasoning-proxy \
  -listen 127.0.0.1:3460 \
  -target https://opencode.ai/zen/go \
  -cache ~/.config/ogc/reasoning-cache.json
```

Set the OGC OpenAI upstream base URL to `http://127.0.0.1:3460/v1`, then start:

```bash
./bin/ogc serve
```

Keys can be managed in `~/.config/ogc/config.json` through the `accounts`
array or from the localhost administration UI. Do not commit real keys or the
reasoning cache.
