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
    future := fanout.ExecuteManyActivities[EmailRequest, EmailResult](
        ctx,
        SendEmail,
        emails,
        fanout.WithConcurrency[EmailRequest](100), // 100 concurrent activities
    )

    results, err := future.Get(ctx)
    return err
}
```

**That's it!** No child workflows, no manual queuing, no shard management.

---

## API Reference

### `ExecuteManyActivities[T, R any]()`

```go
func ExecuteManyActivities[T, R any](
    ctx workflow.Context,
    activity any,               // Your activity function
    items []T,                  // Data to process (or nil if using ParamFactory)
    options ...Option[T],       // Optional configuration
) BulkFuture[R]
```

**Type Parameters:**
- `T`: Input type (what each activity receives)
- `R`: Result type (what each activity returns)

**Parameters:**
- `ctx`: Workflow context with activity options set
- `activity`: Your activity function (must accept `(context.Context, T)` and return `(R, error)`)
- `items`: Slice of items to process, OR `nil` if using `ParamFactory`
- `options`: Optional configuration (see below)

**Returns:**
- `BulkFuture[R]`: A future that resolves to `[]R` when all activities complete

---

### Configuration Options

#### `WithConcurrency[T](n int)`

Control how many activities run concurrently.

```go
fanout.WithConcurrency[EmailRequest](100) // 100 concurrent activities
```

**Default:** 10

**Guidelines:**
- **50-100**: Safe for most use cases
- **200-500**: High volume, requires good heartbeat management
- **500+**: Risky, test thoroughly

#### `WithParamFactory[T](factory ParamFactory[T])`

Generate activity parameters dynamically (for offset/limit patterns).

```go
type ParamFactory[T] interface {
    Generate(index int) T  // Generate params for activity at index
    Count() int           // Total number of activities to create
}
```

**Example:** Process 100,000 items in chunks of 1,000

```go
type CatalogFactory struct {
    totalItems int
    chunkSize  int
}

func (f *CatalogFactory) Generate(index int) BuildParams {
    return BuildParams{
        Offset: index * f.chunkSize,
        Limit:  f.chunkSize,
    }
}

func (f *CatalogFactory) Count() int {
    return (f.totalItems + f.chunkSize - 1) / f.chunkSize
}

// Usage
future := fanout.ExecuteManyActivities[BuildParams, BuildResult](
    ctx,
    BuildCatalog,
    nil, // No items, ParamFactory generates params
    fanout.WithConcurrency[BuildParams](50),
    fanout.WithParamFactory[BuildParams](&CatalogFactory{
        totalItems: 100000,
        chunkSize:  1000,
    }),
)
```

---

### `BulkFuture[R]`

The return type from `ExecuteManyActivities`.

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
future := fanout.ExecuteManyActivities(...)

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

future := fanout.ExecuteManyActivities[UserID, DeleteResult](
    ctx,
    DeleteUser,
    users,
    fanout.WithConcurrency[UserID](200),
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

future := fanout.ExecuteManyActivities[EmailBatch, BatchResult](
    ctx,
    SendEmailBatch,
    batches,
    fanout.WithConcurrency[EmailBatch](20),
)
```

### Pattern 3: Offset/Limit (Advanced)

**Use when:** Activity fetches data based on offset/limit, not actual data

```go
// Process 1M database records in chunks of 10,000
future := fanout.ExecuteManyActivities[ProcessParams, ProcessResult](
    ctx,
    ProcessRecords,
    nil, // No data provided
    fanout.WithConcurrency[ProcessParams](100),
    fanout.WithParamFactory[ProcessParams](&OffsetLimitFactory{
        totalRecords: 1_000_000,
        chunkSize:    10_000,
    }),
)
```

**Activity receives:**
```go
func ProcessRecords(ctx context.Context, params ProcessParams) (ProcessResult, error) {
    // params = {Offset: 0, Limit: 10000}
    // params = {Offset: 10000, Limit: 10000}
    // params = {Offset: 20000, Limit: 10000}
    // ... etc
}
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

future := fanout.ExecuteManyActivities(ctx, ...)
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
- ✅ **< 1,000 activities**: Safe in parent workflow
- ⚠️ **1,000 - 10,000 activities**: Monitor history size
- ❌ **> 10,000 activities**: Consider child workflows (future feature)

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
- 🔴 No concurrency control → shard overload
- 🔴 No error aggregation
- 🔴 Verbose and error-prone

### After (With Fanout)

```go
future := fanout.ExecuteManyActivities[EmailRequest, EmailResult](
    ctx,
    SendEmail,
    emails,
    fanout.WithConcurrency[EmailRequest](100),
)

results, err := future.Get(ctx)
```

**Benefits:**
- ✅ Automatic concurrency control
- ✅ Error aggregation with multierr
- ✅ Clean, maintainable code
- ✅ Type-safe with generics

---

## FAQ

### Q: How many activities can I safely create?

**A:** Depends on:
- Concurrency limit (100 = safer than 500)
- Activity duration (faster = better)
- Heartbeat frequency (less = better)

**Safe defaults:** 1,000-5,000 activities with 100 concurrency

---

### Q: What about child workflows?

**A:** This package focuses on activities in a **single workflow**. Child workflow fanout is a future feature.

For now, if you need 100,000+ activities, manually create child workflows with this API inside each child.

---

### Q: Does this work with local activities?

**A:** Not yet, but could be added. File an issue if you need it!

---

### Q: What if I need streaming results?

**A:** Currently, `Get()` waits for all activities to complete. For streaming, you'd need to access individual futures (not supported in this API yet).

---

## Examples

See [examples_test.go](./examples_test.go) for complete working examples:

1. **Simple email sending** - 10,000 items
2. **Bulk user deletion** - 50,000 items
3. **Catalog building** - ParamFactory with offset/limit
4. **Pre-batched data** - User-defined batches
5. **Progress tracking** - Using `IsReady()`
6. **Order fulfillment** - Real-world error handling

---

## Contributing

This is an experimental package (`x/fanout`). Feedback welcome!

**Known limitations:**
- No child workflow fanout (yet)
- No local activity support (yet)
- No streaming results (yet)

**Roadmap:**
- Automatic child workflow fanout for 100,000+ activities
- Activity retry budget management
- Better observability/metrics
