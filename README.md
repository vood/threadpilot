# threadpilot

Browser-first Reddit automation as a Go library plus CLI.

Created by the founder of [clawmaker.dev](https://clawmaker.dev), [writingmate.ai](https://writingmate.ai), [aidictation.com](https://aidictation.com), and [mentioned.to](https://mentioned.to).

## Install CLI

Use a release binary from:

`https://github.com/vood/threadpilot/releases`

Or build locally:

```bash
go test ./...
go build -o dist/threadpilot ./cmd/threadpilot
./dist/threadpilot --help
```

## Use As Library

```go
package main

import (
  "log"

  "github.com/vood/threadpilot"
)

func main() {
  if err := threadpilot.Run([]string{"whoami"}); err != nil {
    log.Fatal(err)
  }
}
```

## Build From Source

```bash
go test ./...
go build -o dist/threadpilot ./cmd/threadpilot
```

## Browser Compatibility

`threadpilot` standardizes on Chrome DevTools Protocol attachment.

It can:

- attach to a local Chrome/Chromium/Edge instance via DevTools HTTP URL
- attach to a local remote-debugging port (for example `http://127.0.0.1:9222`)
- attach directly to an existing WebSocket endpoint
- attach to GoLogin-style cloud browser endpoints
- launch/reuse a local Chromium-compatible browser itself when no endpoint is provided

Examples:

```bash
threadpilot --browser-debug-url http://127.0.0.1:9222 whoami
threadpilot --browser-ws-endpoint \"$GOLOGIN_WS_URL\" whoami
```

Environment shortcuts:

- `THREADPILOT_BROWSER_WS_URL`
- `REDDIT_BROWSER_WS_URL`
- `GOLOGIN_WS_URL`
- `GOLOGIN_WS_ENDPOINT`
- `THREADPILOT_BROWSER_DEBUG_URL`
- `REDDIT_BROWSER_DEBUG_URL`
- `GOLOGIN_DEBUG_URL`

## Core Commands

- `login`, `whoami`
- `my-comments`, `my-replies`, `my-posts`
- `my-subreddits`, `subscribe`
- `read`, `search`, `rules`
- `like`, `post`

## Notes

- Persistent profile default: `~/.threadpilot-profile`
- `--proxy` is supported for HTTP and browser-backed workflows.
