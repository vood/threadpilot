# threadpilot

Browser-first Reddit automation as a Go library plus CLI.

Created by the founder of [clawmaker.dev](https://clawmaker.dev), [writingmate.ai](https://writingmate.ai), [aidictation.com](https://aidictation.com), and [mentioned.to](https://mentioned.to).

## Install CLI

```bash
go install github.com/vood/threadpilot/cmd/threadpilot@latest
threadpilot --help
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

## Core Commands

- `login`, `whoami`
- `my-comments`, `my-replies`, `my-posts`
- `my-subreddits`, `subscribe`
- `read`, `search`, `rules`
- `like`, `post`

## Notes

- Persistent profile default: `~/.threadpilot-profile`
- `--proxy` is supported for HTTP and browser-backed workflows.
