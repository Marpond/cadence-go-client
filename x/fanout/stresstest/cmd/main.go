package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/cadence/x/fanout/stresstest"
)

func main() {
	var (
		numItems        int
		simulatedWorkMs int
		serviceAddr     string
		domain          string
		reportFile      string
		enableHeartbeat bool
	)

	flag.IntVar(&numItems, "items", 100, "Number of items to process")
	flag.IntVar(&simulatedWorkMs, "work-ms", 10, "Simulated work duration per activity in milliseconds")
	flag.StringVar(&serviceAddr, "service-addr", "127.0.0.1:7933", "Cadence service address")
	flag.StringVar(&domain, "domain", stresstest.DefaultDomain, "Cadence domain")
	flag.StringVar(&reportFile, "report", "", "Output file for JSON report (optional)")
	flag.BoolVar(&enableHeartbeat, "heartbeat", false, "Enable activity heartbeating (critical for testing at scale)")
	flag.Parse()

	config := stresstest.RunnerConfig{
		ServiceAddr: serviceAddr,
		Domain:      domain,
	}

	runner, err := stresstest.NewRunner(config)
	if err != nil {
		log.Fatalf("Failed to create runner: %v", err)
	}

	if err := runner.Connect(); err != nil {
		log.Fatalf("Failed to connect to Cadence: %v", err)
	}
	defer runner.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nReceived shutdown signal...")
		cancel()
	}()

	// Setup domain
	if err := runner.SetupDomain(ctx); err != nil {
		log.Fatalf("Failed to setup domain: %v", err)
	}

	// Run comparison
	fmt.Printf("Running comparison test with %d items...\n", numItems)
	if enableHeartbeat {
		fmt.Println("⚠️  HEARTBEAT ENABLED - Activities will send periodic heartbeats")
		fmt.Println("   This tests the hot shard problem at scale!")
	}
	fmt.Println("This will run multiple scenarios and compare their performance.")
	fmt.Println()

	// Start worker in background
	worker, err := runner.StartWorker()
	if err != nil {
		log.Fatalf("Failed to start worker: %v", err)
	}
	defer worker.Stop()

	// Give worker time to register
	time.Sleep(2 * time.Second)

	report, err := runner.RunComparison(ctx, numItems, simulatedWorkMs, enableHeartbeat)
	if err != nil {
		log.Fatalf("Comparison test failed: %v", err)
	}

	runner.PrintReport(report)

	if reportFile != "" {
		if err := runner.SaveReport(report, reportFile); err != nil {
			log.Printf("Failed to save report: %v", err)
		} else {
			fmt.Printf("\nReport saved to: %s\n", reportFile)
		}
	}
}
