# Background workers

Read this before running a long-lived loop (a bot holding a connection, a poller, a leader-election loop) in a process.

`server/worker` wraps any long-running loop as a `keel.Server`, so it joins the application lifecycle. It is for loops
that are neither message-driven nor schedule-driven.

- **Scheduled work uses kasynq cron (`RegisterCron`)**, not a ticker here: cron tasks have deduplication, retries and
  status queries (see [Tasks](tasks.md)); a `Runner` is only a function that runs until its `ctx` is cancelled.
- **Message-driven work uses a knats consumer** (see [Messaging](messaging.md)). A worker drives itself.

## Use

A `Runner` is the work. When it returns, the server is finished; a non-nil error shuts the whole application down
gracefully. A panic is recovered and turned into an error instead of crashing the process.

```go
import "github.com/go-keel/keel/server/worker"

func NewFeedPoller(feed *biz.FeedUseCase) worker.Runner {
    return func(ctx context.Context) error {
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return nil // shutdown requested: exit normally
            case <-ticker.C:
                if err := feed.Poll(ctx); err != nil {
                    slog.ErrorContext(ctx, "poll failed", "error", err)
                }
            }
        }
    }
}

// di.go
app, err := factory.New(worker.Server(NewFeedPoller(feedUC)))
```

## Stopping

`Stop` cancels the runner's `ctx` and waits, within the budget from `app.stop_timeout` (see
[Configuration](config.md)), for the runner to return; past the budget it returns an error rather than pretending
success. **The runner must watch `ctx.Done()` and finish promptly**, as the `select` above does; work in flight when
the signal arrives either completes or is cancelled before the runner returns.

Keel cannot force this. Kratos's `App.Run` waits for `Start` to return before it considers the server stopped, so a
runner that ignores `ctx.Done()` keeps the process from stopping; the error from `Stop` only makes that visible.
