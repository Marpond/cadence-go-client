package stresstest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/cadence/.gen/go/cadence/workflowserviceclient"
	"go.uber.org/cadence/.gen/go/shared"
	"go.uber.org/cadence/client"
	"go.uber.org/cadence/worker"
	"go.uber.org/yarpc"
	"go.uber.org/yarpc/transport/tchannel"
	"go.uber.org/zap"
)

const (
	// DefaultDomain is the cadence domain for stress tests
	DefaultDomain = "fanout-stress-test"

	// DefaultServiceName is the cadence frontend service name
	DefaultServiceName = "cadence-frontend"

	// DefaultServiceAddr is the default cadence server address
	DefaultServiceAddr = "127.0.0.1:7933"
)

// RunnerConfig configures the stress test runner
type RunnerConfig struct {
	ServiceName string
	ServiceAddr string
	Domain      string

	// Worker configuration
	MaxConcurrentActivityExecutionSize int
	MaxConcurrentDecisionTaskSize      int
}

// Runner orchestrates stress test execution
type Runner struct {
	config       RunnerConfig
	logger       *zap.Logger
	client       client.Client
	domainClient client.DomainClient
	service      workflowserviceclient.Interface
	dispatcher   *yarpc.Dispatcher
}

// NewRunner creates a new stress test runner
func NewRunner(config RunnerConfig) (*Runner, error) {
	logger, err := zap.NewDevelopment()
	if err != nil {
		return nil, fmt.Errorf("failed to create logger: %w", err)
	}

	// Set defaults
	if config.ServiceName == "" {
		config.ServiceName = DefaultServiceName
	}
	if config.ServiceAddr == "" {
		config.ServiceAddr = getEnv("SERVICE_ADDR", DefaultServiceAddr)
	}
	if config.Domain == "" {
		config.Domain = DefaultDomain
	}
	if config.MaxConcurrentActivityExecutionSize <= 0 {
		config.MaxConcurrentActivityExecutionSize = 1000
	}
	if config.MaxConcurrentDecisionTaskSize <= 0 {
		config.MaxConcurrentDecisionTaskSize = 50
	}

	return &Runner{
		config: config,
		logger: logger,
	}, nil
}

// Connect establishes connection to Cadence server
func (r *Runner) Connect() error {
	transport, err := tchannel.NewTransport(tchannel.ServiceName("fanout-stress-test"))
	if err != nil {
		return fmt.Errorf("failed to create transport: %w", err)
	}

	outbound := transport.NewSingleOutbound(r.config.ServiceAddr)
	dispatcher := yarpc.NewDispatcher(yarpc.Config{
		Name: "fanout-stress-test",
		Outbounds: yarpc.Outbounds{
			r.config.ServiceName: {
				Unary: outbound,
			},
		},
	})

	if err := dispatcher.Start(); err != nil {
		return fmt.Errorf("failed to start dispatcher: %w", err)
	}

	r.dispatcher = dispatcher
	r.service = workflowserviceclient.New(dispatcher.ClientConfig(r.config.ServiceName))
	r.client = client.NewClient(r.service, r.config.Domain, &client.Options{})
	r.domainClient = client.NewDomainClient(r.service, &client.Options{})

	return nil
}

// Close releases resources
func (r *Runner) Close() {
	if r.dispatcher != nil {
		r.dispatcher.Stop()
	}
}

// SetupDomain creates the test domain if it doesn't exist
func (r *Runner) SetupDomain(ctx context.Context) error {
	_, err := r.domainClient.Describe(ctx, r.config.Domain)
	if err == nil {
		r.logger.Info("Domain already exists", zap.String("domain", r.config.Domain))
		return nil
	}

	// Domain doesn't exist, create it
	retention := int32(1) // 1 day retention for tests
	isGlobalDomain := false

	req := &shared.RegisterDomainRequest{
		Name:                                   &r.config.Domain,
		WorkflowExecutionRetentionPeriodInDays: &retention,
		IsGlobalDomain:                         &isGlobalDomain,
	}

	err = r.domainClient.Register(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to register domain: %w", err)
	}

	r.logger.Info("Created domain", zap.String("domain", r.config.Domain))

	// Give the domain a moment to propagate
	time.Sleep(2 * time.Second)
	return nil
}

// StartWorker starts a worker for stress tests
func (r *Runner) StartWorker() (worker.Worker, error) {
	workerOptions := worker.Options{
		Logger:                                 r.logger,
		MaxConcurrentActivityExecutionSize:     r.config.MaxConcurrentActivityExecutionSize,
		MaxConcurrentDecisionTaskExecutionSize: r.config.MaxConcurrentDecisionTaskSize,
	}

	w := worker.New(r.service, r.config.Domain, TaskListName, workerOptions)

	RegisterWorkflows(w)
	RegisterActivities(w)

	if err := w.Start(); err != nil {
		return nil, fmt.Errorf("failed to start worker: %w", err)
	}

	r.logger.Info("Worker started", zap.String("taskList", TaskListName))
	return w, nil
}

// TestScenario represents a single test scenario
type TestScenario struct {
	Name         string
	WorkflowType string
	Config       StressTestConfig
}

// TestReport contains results from all test scenarios
type TestReport struct {
	StartTime  time.Time                    `json:"startTime"`
	EndTime    time.Time                    `json:"endTime"`
	Scenarios  []TestScenarioResult         `json:"scenarios"`
	Comparison map[string]ComparisonMetrics `json:"comparison"`
}

// TestScenarioResult contains results for a single scenario
type TestScenarioResult struct {
	Scenario   TestScenario     `json:"scenario"`
	Result     StressTestResult `json:"result"`
	Error      string           `json:"error,omitempty"`
	WorkflowID string           `json:"workflowId"`
	RunID      string           `json:"runId"`
}

// ComparisonMetrics shows comparison between scenarios
type ComparisonMetrics struct {
	TotalDuration  time.Duration `json:"totalDuration"`
	SpeedupVsNaive float64       `json:"speedupVsNaive"`
	SuccessRate    float64       `json:"successRate"`
}

// RunScenario executes a single test scenario
func (r *Runner) RunScenario(ctx context.Context, scenario TestScenario) TestScenarioResult {
	result := TestScenarioResult{
		Scenario: scenario,
	}

	workflowID := fmt.Sprintf("stress-test-%s-%d", scenario.Name, time.Now().UnixNano())

	opts := client.StartWorkflowOptions{
		ID:                              workflowID,
		TaskList:                        TaskListName,
		ExecutionStartToCloseTimeout:    4 * time.Hour,
		DecisionTaskStartToCloseTimeout: 10 * time.Minute,
	}

	r.logger.Info("Starting test scenario",
		zap.String("name", scenario.Name),
		zap.String("workflowType", scenario.WorkflowType),
		zap.Int("numItems", scenario.Config.NumItems),
	)

	we, err := r.client.StartWorkflow(ctx, opts, scenario.WorkflowType, scenario.Config)
	if err != nil {
		result.Error = fmt.Sprintf("failed to start workflow: %v", err)
		return result
	}

	result.WorkflowID = we.ID
	result.RunID = we.RunID

	r.logger.Info("Workflow started, waiting for completion",
		zap.String("workflowId", we.ID),
		zap.String("runId", we.RunID),
	)

	// Wait for workflow completion
	var testResult StressTestResult
	err = r.client.GetWorkflow(ctx, we.ID, we.RunID).Get(ctx, &testResult)
	if err != nil {
		result.Error = fmt.Sprintf("workflow failed: %v", err)
		return result
	}

	result.Result = testResult
	r.logger.Info("Scenario completed",
		zap.String("name", scenario.Name),
		zap.Duration("totalDuration", testResult.TotalDuration),
		zap.Int("successCount", testResult.SuccessCount),
		zap.Int("failureCount", testResult.FailureCount),
	)

	return result
}

// RunComparison runs a comprehensive comparison test
func (r *Runner) RunComparison(ctx context.Context, numItems int, simulatedWorkMs int, enableHeartbeat bool) (*TestReport, error) {
	report := &TestReport{
		StartTime:  time.Now(),
		Scenarios:  make([]TestScenarioResult, 0),
		Comparison: make(map[string]ComparisonMetrics),
	}

	baseConfig := StressTestConfig{
		NumItems:                numItems,
		MaxConcurrentActivities: 50,
		PayloadSizeBytes:        1024,
		SimulatedWorkDuration:   time.Duration(simulatedWorkMs) * time.Millisecond,
		EnableHeartbeat:         enableHeartbeat,
		HeartbeatIntervalMs:     100, // 100ms heartbeat interval
		// MaxActivitiesPerWorkflow: threshold for fanning out to child workflows
		// Set to 100 so that 500 items -> 5 child workflows (100 each)
		// This distributes load across multiple shards!
		MaxActivitiesPerWorkflow: 100,
		MaxConcurrentChildren:    5, // Run 5 child workflows in parallel
	}

	heartbeatSuffix := ""
	if enableHeartbeat {
		heartbeatSuffix = "-heartbeat"
	}

	scenarios := []TestScenario{
		// WARNING: Only run naive test with small numbers, it can overwhelm the system
		{
			Name:         "naive-all-at-once" + heartbeatSuffix,
			WorkflowType: NaiveWorkflowName,
			Config:       baseConfig,
		},
		{
			Name:         "naive-with-concurrency-control" + heartbeatSuffix,
			WorkflowType: NaiveWithConcurrencyControlName,
			Config:       baseConfig,
		},
		{
			Name:         "fanout-in-place" + heartbeatSuffix,
			WorkflowType: FanoutInPlaceWorkflowName,
			Config:       baseConfig,
		},
		{
			Name:         "fanout-with-children" + heartbeatSuffix,
			WorkflowType: FanoutWithChildrenWorkflowName,
			Config:       baseConfig,
		},
	}

	// Run scenarios sequentially to avoid interference
	for _, scenario := range scenarios {
		// NOTE: We intentionally DO NOT skip naive tests at high item counts
		// The whole point is to demonstrate that it breaks/degrades at scale!
		result := r.RunScenario(ctx, scenario)
		report.Scenarios = append(report.Scenarios, result)

		// Small delay between tests
		time.Sleep(5 * time.Second)
	}

	// Calculate comparison metrics
	report.EndTime = time.Now()
	r.calculateComparisons(report)

	return report, nil
}

func (r *Runner) calculateComparisons(report *TestReport) {
	var naiveDuration time.Duration

	for _, sr := range report.Scenarios {
		if sr.Scenario.WorkflowType == NaiveWorkflowName && sr.Error == "" {
			naiveDuration = sr.Result.TotalDuration
			break
		}
	}

	for _, sr := range report.Scenarios {
		if sr.Error != "" {
			continue
		}

		metrics := ComparisonMetrics{
			TotalDuration: sr.Result.TotalDuration,
		}

		if sr.Result.NumItems > 0 {
			metrics.SuccessRate = float64(sr.Result.SuccessCount) / float64(sr.Result.NumItems) * 100
		}

		if naiveDuration > 0 && sr.Result.TotalDuration > 0 {
			metrics.SpeedupVsNaive = float64(naiveDuration) / float64(sr.Result.TotalDuration)
		}

		report.Comparison[sr.Scenario.Name] = metrics
	}
}

// PrintReport prints a formatted report to stdout
func (r *Runner) PrintReport(report *TestReport) {
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("FANOUT STRESS TEST REPORT")
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("Start Time: %s\n", report.StartTime.Format(time.RFC3339))
	fmt.Printf("End Time:   %s\n", report.EndTime.Format(time.RFC3339))
	fmt.Printf("Duration:   %s\n", report.EndTime.Sub(report.StartTime))
	fmt.Println()

	fmt.Println("SCENARIO RESULTS:")
	fmt.Println(strings.Repeat("-", 80))
	fmt.Printf("%-35s %10s %10s %10s %8s\n", "Scenario", "Duration", "Success", "Failed", "Rate")
	fmt.Println(strings.Repeat("-", 80))

	for _, sr := range report.Scenarios {
		if sr.Error != "" {
			fmt.Printf("%-35s ERROR: %s\n", sr.Scenario.Name, sr.Error)
			continue
		}

		successRate := float64(sr.Result.SuccessCount) / float64(sr.Result.NumItems) * 100
		fmt.Printf("%-35s %10s %10d %10d %7.1f%%\n",
			sr.Scenario.Name,
			sr.Result.TotalDuration.Round(time.Millisecond),
			sr.Result.SuccessCount,
			sr.Result.FailureCount,
			successRate,
		)
	}

	fmt.Println()
	fmt.Println("COMPARISON METRICS:")
	fmt.Println(strings.Repeat("-", 80))
	for name, metrics := range report.Comparison {
		fmt.Printf("%-35s Duration: %s, Success Rate: %.1f%%",
			name,
			metrics.TotalDuration.Round(time.Millisecond),
			metrics.SuccessRate,
		)
		if metrics.SpeedupVsNaive > 0 {
			fmt.Printf(", Speedup vs Naive: %.2fx", metrics.SpeedupVsNaive)
		}
		fmt.Println()
	}

	fmt.Println(strings.Repeat("=", 80))
}

// SaveReport saves the report to a JSON file
func (r *Runner) SaveReport(report *TestReport, filename string) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filename, data, 0644)
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
