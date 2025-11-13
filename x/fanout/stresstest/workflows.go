package stresstest

import (
	"context"
	"time"

	"go.uber.org/cadence/activity"
	"go.uber.org/cadence/workflow"
	"go.uber.org/zap"

	"go.uber.org/cadence/x/fanout"
)

const (
	// Workflow names
	NaiveWorkflowName               = "fanout-stress-naive-workflow"
	FanoutInPlaceWorkflowName       = "fanout-stress-fanout-inplace-workflow"
	FanoutWithChildrenWorkflowName  = "fanout-stress-fanout-children-workflow"
	NaiveWithConcurrencyControlName = "fanout-stress-naive-controlled-workflow"
	FanoutChildWorkflowName         = "fanout-stress-child-workflow"

	// Activity names
	ProcessItemActivityName = "fanout-stress-process-item-activity"

	// Task list
	TaskListName = "fanout-stress-test-tasklist"
)

// StressTestConfig contains parameters for stress tests
type StressTestConfig struct {
	NumItems                int           `json:"numItems"`
	MaxConcurrentActivities int           `json:"maxConcurrentActivities"`
	PayloadSizeBytes        int           `json:"payloadSizeBytes"`
	SimulatedWorkDuration   time.Duration `json:"simulatedWorkDuration"`

	// Heartbeat configuration - critical for testing at scale!
	// When true, activities will heartbeat every HeartbeatIntervalMs
	// This is what causes issues around 500 activities due to:
	// - Each heartbeat = write to history
	// - Hot shard problem (all heartbeats hit same workflow's shard)
	// - Cassandra partition pressure
	EnableHeartbeat     bool `json:"enableHeartbeat"`
	HeartbeatIntervalMs int  `json:"heartbeatIntervalMs"`

	// Fanout specific options
	MaxActivitiesPerWorkflow int `json:"maxActivitiesPerWorkflow"`
	MaxConcurrentChildren    int `json:"maxConcurrentChildren"`
}

// StressTestResult contains the result of a stress test run
type StressTestResult struct {
	WorkflowType    string        `json:"workflowType"`
	NumItems        int           `json:"numItems"`
	TotalDuration   time.Duration `json:"totalDuration"`
	SuccessCount    int           `json:"successCount"`
	FailureCount    int           `json:"failureCount"`
	AvgActivityTime time.Duration `json:"avgActivityTime"`
}

// ProcessItemInput is the input for each activity
type ProcessItemInput struct {
	ItemID            int    `json:"itemId"`
	Payload           []byte `json:"payload"`
	SimulatedWorkMs   int    `json:"simulatedWorkMs"`
	EnableHeartbeat   bool   `json:"enableHeartbeat"`
	HeartbeatInterval int    `json:"heartbeatIntervalMs"` // milliseconds between heartbeats
}

// ProcessItemOutput is the output from each activity
type ProcessItemOutput struct {
	ItemID         int           `json:"itemId"`
	ProcessedAt    time.Time     `json:"processedAt"`
	Duration       time.Duration `json:"duration"`
	HeartbeatCount int           `json:"heartbeatCount"` // Number of heartbeats sent
}

// =============================================================================
// Activities
// =============================================================================

// ProcessItemActivity simulates processing a single item
// When heartbeating is enabled, this activity will send periodic heartbeats
// which is the key behavior that causes issues at scale (500+ activities)
func ProcessItemActivity(ctx context.Context, input ProcessItemInput) (ProcessItemOutput, error) {
	start := time.Now()
	heartbeatCount := 0

	workDuration := time.Duration(input.SimulatedWorkMs) * time.Millisecond
	if workDuration <= 0 {
		workDuration = 10 * time.Millisecond
	}

	if input.EnableHeartbeat && input.HeartbeatInterval > 0 {
		// Heartbeat mode: send periodic heartbeats during work
		// This is what causes hot shard problems at scale!
		// Each heartbeat = ActivityTaskHeartbeat RPC = history event
		heartbeatInterval := time.Duration(input.HeartbeatInterval) * time.Millisecond
		deadline := time.Now().Add(workDuration)

		for time.Now().Before(deadline) {
			// Send heartbeat with progress
			activity.RecordHeartbeat(ctx, heartbeatCount)
			heartbeatCount++

			// Check for cancellation
			if ctx.Err() != nil {
				return ProcessItemOutput{}, ctx.Err()
			}

			// Sleep until next heartbeat or work completion
			sleepDuration := heartbeatInterval
			remaining := time.Until(deadline)
			if remaining < sleepDuration {
				sleepDuration = remaining
			}
			if sleepDuration > 0 {
				time.Sleep(sleepDuration)
			}
		}
	} else {
		// Simple mode: just sleep
		time.Sleep(workDuration)
	}

	return ProcessItemOutput{
		ItemID:         input.ItemID,
		ProcessedAt:    time.Now(),
		Duration:       time.Since(start),
		HeartbeatCount: heartbeatCount,
	}, nil
}

// =============================================================================
// Naive Workflow - Anti-pattern: launches all activities at once
// =============================================================================

// NaiveWorkflow launches ALL activities at once without any concurrency control.
// This is the ANTI-PATTERN that can cause:
// - Hot shard problems (all activities belong to the same workflow)
// - Overwhelming the worker with too many pending activities
// - Potential OOM on the history service
// - Very large event history
func NaiveWorkflow(ctx workflow.Context, config StressTestConfig) (StressTestResult, error) {
	startTime := workflow.Now(ctx)
	logger := workflow.GetLogger(ctx)
	logger.Info("Starting naive workflow", zap.Int("numItems", config.NumItems), zap.Bool("heartbeat", config.EnableHeartbeat))

	ao := makeActivityOptions(config)
	ctx = workflow.WithActivityOptions(ctx, ao)

	// Prepare inputs
	inputs := makeInputs(config)

	// ANTI-PATTERN: Launch ALL activities at once
	futures := make([]workflow.Future, len(inputs))
	for i, input := range inputs {
		futures[i] = workflow.ExecuteActivity(ctx, ProcessItemActivityName, input)
	}

	// Wait for all to complete
	successCount := 0
	failureCount := 0
	var totalActivityDuration time.Duration

	for i, f := range futures {
		var output ProcessItemOutput
		if err := f.Get(ctx, &output); err != nil {
			logger.Error("Activity failed", zap.Int("itemId", i), zap.Error(err))
			failureCount++
		} else {
			successCount++
			totalActivityDuration += output.Duration
		}
	}

	result := StressTestResult{
		WorkflowType:  NaiveWorkflowName,
		NumItems:      config.NumItems,
		TotalDuration: workflow.Now(ctx).Sub(startTime),
		SuccessCount:  successCount,
		FailureCount:  failureCount,
	}
	if successCount > 0 {
		result.AvgActivityTime = totalActivityDuration / time.Duration(successCount)
	}

	logger.Info("Naive workflow completed", zap.Any("result", result))
	return result, nil
}

// =============================================================================
// Naive with Manual Concurrency Control - Better but still single workflow
// =============================================================================

// NaiveWithConcurrencyControlWorkflow uses a selector pattern to limit concurrency
// but still runs everything in a single workflow (no child workflow fanout)
func NaiveWithConcurrencyControlWorkflow(ctx workflow.Context, config StressTestConfig) (StressTestResult, error) {
	startTime := workflow.Now(ctx)
	logger := workflow.GetLogger(ctx)
	logger.Info("Starting naive controlled workflow", zap.Int("numItems", config.NumItems), zap.Bool("heartbeat", config.EnableHeartbeat))

	ao := makeActivityOptions(config)
	ctx = workflow.WithActivityOptions(ctx, ao)

	inputs := makeInputs(config)
	maxConcurrency := config.MaxConcurrentActivities
	if maxConcurrency <= 0 {
		maxConcurrency = 10
	}

	successCount := 0
	failureCount := 0
	var totalActivityDuration time.Duration

	// Process in batches using selector
	pendingFutures := make([]workflow.Future, 0, maxConcurrency)
	nextItemIdx := 0

	for nextItemIdx < len(inputs) || len(pendingFutures) > 0 {
		// Launch new activities up to concurrency limit
		for len(pendingFutures) < maxConcurrency && nextItemIdx < len(inputs) {
			f := workflow.ExecuteActivity(ctx, ProcessItemActivityName, inputs[nextItemIdx])
			pendingFutures = append(pendingFutures, f)
			nextItemIdx++
		}

		// Wait for at least one to complete
		if len(pendingFutures) > 0 {
			selector := workflow.NewSelector(ctx)
			var completedIdx int
			for i, f := range pendingFutures {
				i, f := i, f
				selector.AddFuture(f, func(f workflow.Future) {
					completedIdx = i
					var output ProcessItemOutput
					if err := f.Get(ctx, &output); err != nil {
						failureCount++
					} else {
						successCount++
						totalActivityDuration += output.Duration
					}
				})
			}
			selector.Select(ctx)

			// Remove completed future
			pendingFutures = append(pendingFutures[:completedIdx], pendingFutures[completedIdx+1:]...)
		}
	}

	result := StressTestResult{
		WorkflowType:  NaiveWithConcurrencyControlName,
		NumItems:      config.NumItems,
		TotalDuration: workflow.Now(ctx).Sub(startTime),
		SuccessCount:  successCount,
		FailureCount:  failureCount,
	}
	if successCount > 0 {
		result.AvgActivityTime = totalActivityDuration / time.Duration(successCount)
	}

	logger.Info("Naive controlled workflow completed", zap.Any("result", result))
	return result, nil
}

// =============================================================================
// Fanout In-Place Workflow - Uses fanout API without child workflows
// =============================================================================

// FanoutInPlaceWorkflow uses the fanout API for in-place execution with concurrency control
func FanoutInPlaceWorkflow(ctx workflow.Context, config StressTestConfig) (StressTestResult, error) {
	startTime := workflow.Now(ctx)
	logger := workflow.GetLogger(ctx)
	logger.Info("Starting fanout in-place workflow", zap.Int("numItems", config.NumItems), zap.Bool("heartbeat", config.EnableHeartbeat))

	ao := makeActivityOptions(config)
	ctx = workflow.WithActivityOptions(ctx, ao)

	inputs := makeInputs(config)
	maxConcurrency := config.MaxConcurrentActivities
	if maxConcurrency <= 0 {
		maxConcurrency = 10
	}

	// Use the fanout API - clean and simple!
	bulkFuture := fanout.ExecuteActivityInBulk[ProcessItemInput, ProcessItemOutput](
		ctx,
		ProcessItemActivityName,
		inputs,
		fanout.WithMaxConcurrentActivities(maxConcurrency),
	)

	results, err := bulkFuture.Get(ctx)

	successCount := 0
	failureCount := 0
	var totalActivityDuration time.Duration

	// Note: err might be a multierr with partial failures
	if err == nil {
		successCount = len(results)
		for _, r := range results {
			totalActivityDuration += r.Duration
		}
	} else {
		// Count successes and failures based on zero values
		for _, r := range results {
			if r.ProcessedAt.IsZero() {
				failureCount++
			} else {
				successCount++
				totalActivityDuration += r.Duration
			}
		}
	}

	result := StressTestResult{
		WorkflowType:  FanoutInPlaceWorkflowName,
		NumItems:      config.NumItems,
		TotalDuration: workflow.Now(ctx).Sub(startTime),
		SuccessCount:  successCount,
		FailureCount:  failureCount,
	}
	if successCount > 0 {
		result.AvgActivityTime = totalActivityDuration / time.Duration(successCount)
	}

	logger.Info("Fanout in-place workflow completed", zap.Any("result", result))
	return result, nil
}

// =============================================================================
// Fanout with Child Workflows - For very large scale operations
// =============================================================================

// FanoutWithChildrenWorkflow uses the fanout API with child workflow distribution.
// This is the RECOMMENDED approach for very large scale operations because:
// - Work is distributed across multiple workflows (different shards)
// - Each child has its own event history (limits history growth)
// - Better failure isolation
func FanoutWithChildrenWorkflow(ctx workflow.Context, config StressTestConfig) (StressTestResult, error) {
	startTime := workflow.Now(ctx)
	logger := workflow.GetLogger(ctx)
	logger.Info("Starting fanout with children workflow", zap.Int("numItems", config.NumItems), zap.Bool("heartbeat", config.EnableHeartbeat))

	ao := makeActivityOptions(config)
	ctx = workflow.WithActivityOptions(ctx, ao)

	childOpts := workflow.ChildWorkflowOptions{
		TaskList:                     TaskListName,
		ExecutionStartToCloseTimeout: 2 * time.Hour,
		TaskStartToCloseTimeout:      time.Minute,
	}

	inputs := makeInputs(config)

	maxConcurrency := config.MaxConcurrentActivities
	if maxConcurrency <= 0 {
		maxConcurrency = 10
	}
	maxPerWorkflow := config.MaxActivitiesPerWorkflow
	if maxPerWorkflow <= 0 {
		maxPerWorkflow = 100
	}
	maxChildren := config.MaxConcurrentChildren
	if maxChildren <= 0 {
		maxChildren = 10
	}

	// Use the fanout API with child workflow options
	bulkFuture := fanout.ExecuteActivityInBulk[ProcessItemInput, ProcessItemOutput](
		ctx,
		ProcessItemActivityName,
		inputs,
		fanout.WithMaxConcurrentActivities(maxConcurrency),
		fanout.WithFanoutOptions(fanout.FanoutOptions{
			MaxActivitiesPerWorkflow: maxPerWorkflow,
			MaxConcurrentChildren:    maxChildren,
			ChildWorkflowOptions:     childOpts,
			ActivityOptions:          ao,
		}),
	)

	results, err := bulkFuture.Get(ctx)

	successCount := 0
	failureCount := 0
	var totalActivityDuration time.Duration

	if err == nil {
		successCount = len(results)
		for _, r := range results {
			totalActivityDuration += r.Duration
		}
	} else {
		for _, r := range results {
			if r.ProcessedAt.IsZero() {
				failureCount++
			} else {
				successCount++
				totalActivityDuration += r.Duration
			}
		}
	}

	result := StressTestResult{
		WorkflowType:  FanoutWithChildrenWorkflowName,
		NumItems:      config.NumItems,
		TotalDuration: workflow.Now(ctx).Sub(startTime),
		SuccessCount:  successCount,
		FailureCount:  failureCount,
	}
	if successCount > 0 {
		result.AvgActivityTime = totalActivityDuration / time.Duration(successCount)
	}

	logger.Info("Fanout with children workflow completed", zap.Any("result", result))
	return result, nil
}

// =============================================================================
// Helper Functions
// =============================================================================

func makeInputs(config StressTestConfig) []ProcessItemInput {
	inputs := make([]ProcessItemInput, config.NumItems)
	payload := make([]byte, config.PayloadSizeBytes)

	simulatedMs := int(config.SimulatedWorkDuration.Milliseconds())
	if simulatedMs < 0 {
		simulatedMs = 0
	}

	heartbeatIntervalMs := config.HeartbeatIntervalMs
	if heartbeatIntervalMs <= 0 {
		heartbeatIntervalMs = 100 // Default 100ms heartbeat interval
	}

	for i := range inputs {
		inputs[i] = ProcessItemInput{
			ItemID:            i,
			Payload:           payload,
			SimulatedWorkMs:   simulatedMs,
			EnableHeartbeat:   config.EnableHeartbeat,
			HeartbeatInterval: heartbeatIntervalMs,
		}
	}
	return inputs
}

// makeActivityOptions creates activity options with optional heartbeat timeout
func makeActivityOptions(config StressTestConfig) workflow.ActivityOptions {
	ao := workflow.ActivityOptions{
		TaskList:               TaskListName,
		ScheduleToStartTimeout: time.Hour,
		StartToCloseTimeout:    5 * time.Minute,
	}

	// If heartbeating is enabled, set a heartbeat timeout
	// This is critical for validating heartbeat behavior at scale
	if config.EnableHeartbeat {
		// Heartbeat timeout must be > heartbeat interval AND allow for work duration
		// The activity sends heartbeats every HeartbeatIntervalMs during work
		// Timeout should give enough slack for network latency + processing
		heartbeatTimeout := time.Duration(config.HeartbeatIntervalMs*5) * time.Millisecond

		// Ensure timeout is at least 2x the work duration to be safe
		minTimeout := config.SimulatedWorkDuration * 2
		if heartbeatTimeout < minTimeout {
			heartbeatTimeout = minTimeout
		}

		// Minimum 5 seconds to avoid flakiness
		if heartbeatTimeout < 5*time.Second {
			heartbeatTimeout = 5 * time.Second
		}
		ao.HeartbeatTimeout = heartbeatTimeout
	}

	return ao
}

// =============================================================================
// Registration
// =============================================================================

// RegisterWorkflows registers all stress test workflows
func RegisterWorkflows(registry interface {
	RegisterWorkflowWithOptions(w interface{}, options workflow.RegisterOptions)
}) {
	registry.RegisterWorkflowWithOptions(NaiveWorkflow, workflow.RegisterOptions{Name: NaiveWorkflowName})
	registry.RegisterWorkflowWithOptions(NaiveWithConcurrencyControlWorkflow, workflow.RegisterOptions{Name: NaiveWithConcurrencyControlName})
	registry.RegisterWorkflowWithOptions(FanoutInPlaceWorkflow, workflow.RegisterOptions{Name: FanoutInPlaceWorkflowName})
	registry.RegisterWorkflowWithOptions(FanoutWithChildrenWorkflow, workflow.RegisterOptions{Name: FanoutWithChildrenWorkflowName})
}

// RegisterActivities registers all stress test activities
func RegisterActivities(registry interface {
	RegisterActivityWithOptions(a interface{}, options activity.RegisterOptions)
}) {
	registry.RegisterActivityWithOptions(ProcessItemActivity, activity.RegisterOptions{Name: ProcessItemActivityName})
}
