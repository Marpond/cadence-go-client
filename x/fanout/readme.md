# Fanout - Execute Many Activities at Scale

A high-level API for executing thousands of activities in Cadence workflows without overwhelming a single shard.

## The Problem This Solves

When you create thousands of activities in a single Cadence workflow, you hit several bottlenecks:

1. **All activities hit the same database shard** (sharding is by workflow ID)
2. **Heartbeats create massive write load** on that single shard
3. **Serialization overhead** slows everything down
4. **Managing concurrency manually** is tedious and error-prone

This package provides a simple API that handles concurrency control, prevents shard overload, and works for both simple and advanced use cases.

---

## Quick Start

### 1. Simple Case: Send 10,000 Emails

```go
import "go.uber.org/cadence/x/fanout"

type EmailRequest struct {
    To      string
    Subject string
    Body    string
}

// Your activity - processes ONE item
func SendEmail(ctx context.Context, email EmailRequest) (EmailResult, error) {
    return emailService.Send(email)
}

// Your workflow - executes MANY activities
func SendBulkEmailsWorkflow(ctx workflow.Context, emails []EmailRequest) error {
    ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
        StartToCloseTimeout: time.Minute,
    })

    // Execute with automatic concurrency control
    future := fanout.ExecuteActivityInBulk[EmailRequest, EmailResult](
        ctx,
        SendEmail,
        emails,
        fanout.WithMaxConcurrentActivities(100), // 100 concurrent activities
    )

    results, err := future.Get(ctx)
    return err
}
```

**That's it!** No child workflows, no manual queuing, no shard management.

---

## API Reference

### `ExecuteActivityInBulk[T, R any]()`

```go
func ExecuteActivityInBulk[T, R any](
    ctx workflow.Context,
    activity any,               // Your activity function
    items []T,                  // Data to process
    options ...Option,          // Optional configuration
) BulkFuture[R]
```

**Type Parameters:**
- `T`: Input type (what each activity receives)
- `R`: Result type (what each activity returns)

**Parameters:**
- `ctx`: Workflow context with activity options set
- `activity`: Your activity function (must accept `(context.Context, T)` and return `(R, error)`)
- `items`: Slice of items to process
- `options`: Optional configuration (see below)

**Returns:**
- `BulkFuture[R]`: A future that resolves to `[]R` when all activities complete

---

### Configuration Options

#### `WithMaxConcurrentActivities(n int)`

Control how many activities run concurrently.

```go
fanout.WithMaxConcurrentActivities(100) // 100 concurrent activities
```

**Default:** 10

**Guidelines:**
- **50-100**: Safe for most use cases
- **200-500**: High volume, requires good heartbeat management
- **500+**: Risky, test thoroughly

#### `WithFanoutOptions(opts FanoutOptions)`

Enable automatic child workflow fanout for very large workloads.

```go
fanout.WithFanoutOptions(fanout.FanoutOptions{
    MaxActivitiesPerWorkflow: 1000,  // Trigger fanout above this threshold
    MaxConcurrentChildren:    10,    // How many child workflows run in parallel
    ChildWorkflowOptions: workflow.ChildWorkflowOptions{
        ExecutionStartToCloseTimeout: time.Hour,
        TaskStartToCloseTimeout:      time.Minute,
    },
    ActivityOptions: workflow.ActivityOptions{  // Required!
        StartToCloseTimeout: time.Minute,
    },
})
```

**Note:** `ActivityOptions` is required when using fanout, as child workflows need to know how to configure activities.

---

### `BulkFuture[R]`

The return type from `ExecuteActivityInBulk`.

```go
type BulkFuture[R] interface {
    IsReady() bool
    Get(ctx workflow.Context) ([]R, error)
}
```

#### `IsReady() bool`

Returns `true` when all activities have completed.

**Use case:** Progress tracking

```go
future := fanout.ExecuteActivityInBulk(...)

for !future.IsReady() {
    workflow.Sleep(ctx, 10*time.Second)
    workflow.GetLogger(ctx).Info("Still processing...")
}

results, err := future.Get(ctx)
```

#### `Get(ctx workflow.Context) ([]R, error)`

Blocks until all activities complete, then returns results.

**Error Handling:**
- Returns `[]R` with **all** results (even if some activities failed)
- Returns `error` with **all** errors combined using `multierr.Append`
- Check individual activity results to see which succeeded/failed

```go
results, err := future.Get(ctx)

// Even if err != nil, results are populated for successful activities
for i, result := range results {
    if result.Success {
        // This activity succeeded
    }
}

// err contains all errors from failed activities
if err != nil {
    workflow.GetLogger(ctx).Error("Some activities failed", "error", err)
}
```

---

## Usage Patterns

### Pattern 1: Simple Items (Most Common)

**Use when:** Each activity processes one independent item

```go
users := []UserID{"user1", "user2", ..., "user10000"}

future := fanout.ExecuteActivityInBulk[UserID, DeleteResult](
    ctx,
    DeleteUser,
    users,
    fanout.WithMaxConcurrentActivities(200),
)

results, err := future.Get(ctx)
```

### Pattern 2: User-Defined Batching

**Use when:** Your activity works better with multiple items at once

```go
// Batch 10,000 emails into 200 batches of 50
batches := make([]EmailBatch, 200)
for i := 0; i < len(emails); i += 50 {
    batches[i/50] = EmailBatch{Emails: emails[i:i+50]}
}

future := fanout.ExecuteActivityInBulk[EmailBatch, BatchResult](
    ctx,
    SendEmailBatch,
    batches,
    fanout.WithMaxConcurrentActivities(20),
)
```

### Pattern 3: Offset/Limit Processing

**Use when:** Activity fetches data based on offset/limit, not actual data

```go
// Process 1M database records in chunks of 10,000
type ProcessParams struct {
    Offset int
    Limit  int
}

totalRecords := 1_000_000
chunkSize := 10_000
params := make([]ProcessParams, 0)

for offset := 0; offset < totalRecords; offset += chunkSize {
    params = append(params, ProcessParams{
        Offset: offset,
        Limit:  chunkSize,
    })
}

future := fanout.ExecuteActivityInBulk[ProcessParams, ProcessResult](
    ctx,
    ProcessRecords,
    params,
    fanout.WithMaxConcurrentActivities(100),
)
```

---

## Best Practices

### 1. Set Activity Options Before Calling

```go
ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
    ScheduleToStartTimeout: time.Hour,
    StartToCloseTimeout:    5 * time.Minute,
    HeartbeatTimeout:       30 * time.Second,  // Important for long activities
    RetryPolicy: &workflow.RetryPolicy{
        MaximumAttempts: 3,
    },
})

future := fanout.ExecuteActivityInBulk(ctx, ...)
```

### 2. Choose Concurrency Wisely

**Too low:** Workflow takes forever  
**Too high:** Database/shard overload

**Safe starting point:** 50-100 concurrent activities

### 3. Use Heartbeats for Long Activities

If your activity takes >30 seconds:

```go
func ProcessLongTask(ctx context.Context, item Item) (Result, error) {
    for i := 0; i < item.Steps; i++ {
        // Do work
        activity.RecordHeartbeat(ctx, i) // Heartbeat every iteration
    }
    return result, nil
}
```

### 4. Handle Partial Failures

```go
results, err := future.Get(ctx)

if err != nil {
    // Log error but check which activities succeeded
    successCount := 0
    for _, result := range results {
        if result.Success {
            successCount++
        }
    }
    
    workflow.GetLogger(ctx).Info("Partial completion",
        "total", len(results),
        "successful", successCount,
    )
}
```

### 5. Don't Create Too Much History

Even with concurrency control, 10,000 activities = 10,000+ history events.

**Guidelines:**
- **< 1,000 activities**: Safe in parent workflow
- **1,000 - 10,000 activities**: Monitor history size, consider fanout
- **> 10,000 activities**: Use `WithFanoutOptions` to distribute across child workflows

---

## Comparison to Manual Approaches

### Before (Manual)

```go
futures := []workflow.Future{}

// No concurrency control - all 10,000 start immediately!
for _, email := range emails {
    future := workflow.ExecuteActivity(ctx, SendEmail, email)
    futures = append(futures, future)
}

// Manually wait for all
for _, f := range futures {
    var result EmailResult
    if err := f.Get(ctx, &result); err != nil {
        // Handle error
    }
}
```

**Problems:**
- No concurrency control leads to shard overload
- No error aggregation
- Verbose and error-prone

### After (With Fanout)

```go
future := fanout.ExecuteActivityInBulk[EmailRequest, EmailResult](
    ctx,
    SendEmail,
    emails,
    fanout.WithMaxConcurrentActivities(100),
)

results, err := future.Get(ctx)
```

**Benefits:**
- Automatic concurrency control
- Error aggregation with multierr
- Clean, maintainable code
- Type-safe with generics

---

## FAQ

### Q: How many activities can I safely create?

**A:** Depends on:
- Concurrency limit (100 = safer than 500)
- Activity duration (faster = better)
- Heartbeat frequency (less = better)

**Safe defaults:** 1,000-5,000 activities with 100 concurrency

---

### Q: When should I use child workflow fanout?

**A:** Use `WithFanoutOptions` when:
- You have 10,000+ activities
- You're seeing shard hotspotting issues
- You want to distribute load across multiple workflows

Child workflows each get their own shard, spreading the database load.

---

### Q: Does this work with local activities?

**A:** Not yet, but could be added. File an issue if you need it!

---

### Q: What if I need streaming results?

**A:** Currently, `Get()` waits for all activities to complete. For streaming, you'd need to access individual futures (not supported in this API yet).

---

## Contributing

This is an experimental package (`x/fanout`). Feedback welcome!

**Known limitations:**
- No local activity support (yet)
- No streaming results (yet)

**Roadmap:**
- Activity retry budget management
- Better observability/metrics
