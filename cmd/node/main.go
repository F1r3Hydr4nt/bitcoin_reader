package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path"
	"sync"
	"syscall"
	"time"

	"github.com/tokenized/bitcoin_reader"
	"github.com/tokenized/bitcoin_reader/headers"
	"github.com/tokenized/config"
	"github.com/tokenized/logger"
	"github.com/tokenized/pkg/bitcoin"
	"github.com/tokenized/pkg/storage"
	"github.com/tokenized/threads"

	"github.com/pkg/errors"
)

var (
	buildVersion = "unknown"
	buildDate    = "unknown"
	buildUser    = "unknown"
)

func main() {
	// ---------------------------------------------------------------------------------------------
	// Logging

	logPath := "./tmp/node/node.log"
	if len(logPath) > 0 {
		os.MkdirAll(path.Dir(logPath), os.ModePerm)
	}
	isDevelopment := false

	logConfig := logger.NewConfig(isDevelopment, false, logPath)

	ctx := logger.ContextWithLogConfig(context.Background(), logConfig)

	logger.Info(ctx, "Started : Application Initializing")
	defer logger.Info(ctx, "Completed")

	logger.Info(ctx, "Build %v (%v on %v)", buildVersion, buildUser, buildDate)

	// ---------------------------------------------------------------------------------------------
	// Storage

	store, err := storage.CreateStorage("standalone", "./tmp/node", 5, 100)
	if err != nil {
		logger.Fatal(ctx, "Failed to create storage : %s", err)
	}

	headers := headers.NewRepository(headers.DefaultConfig(), store)
	peers := bitcoin_reader.NewPeerRepository(store, "")

	if err := headers.Load(ctx); err != nil {
		logger.Fatal(ctx, "Failed to load headers : %s", err)
	}

	if err := peers.Load(ctx); err != nil {
		logger.Fatal(ctx, "Failed to load peers : %s", err)
	}

	if peers.Count() == 0 {
		peers.LoadSeeds(ctx, bitcoin.MainNet)
	}

	var wait sync.WaitGroup
	var stopper threads.StopCombiner

	stopper.Add(headers)

	// ---------------------------------------------------------------------------------------------
	// Node Manager (Bitcoin P2P)

	userAgent := fmt.Sprintf("/Tokenized/Spynode:Test-%s/", buildVersion)
	logger.Info(ctx, "User Agent : %s", userAgent)

	nodeConfig := &bitcoin_reader.Config{
		Network:                 bitcoin.MainNet,
		Timeout:                 config.NewDuration(time.Hour),
		ScanCount:               500,
		StartupDelay:            config.NewDuration(time.Second * 20),
		ConcurrentBlockRequests: 2,
		DesiredNodeCount:        50,
		BlockRequestDelay:       config.NewDuration(time.Second * 5),
	}
	manager := bitcoin_reader.NewNodeManager(userAgent, nodeConfig, headers, peers)
	managerThread, managerComplete := threads.NewInterruptableThreadComplete("Node Manager",
		manager.Run, &wait)
	stopper.Add(managerThread)

	// ---------------------------------------------------------------------------------------------
	// Processing

	// processor := platform.NewMockDataProcessor()

	txManager := bitcoin_reader.NewTxManager(2 * time.Second)
	// txManager.SetTxProcessor(processor)
	manager.SetTxManager(txManager)
	stopper.Add(txManager)

	processTxThread, processTxComplete := threads.NewUninterruptableThreadComplete("Process Txs",
		txManager.Run, &wait)
	stopper.Add(txManager)

	// blockManager := bitcoin_reader.NewBlockManager(store, manager,
	// nodeConfig.ConcurrentBlockRequests, nodeConfig.BlockRequestDelay)
	// manager.SetBlockManager(blockTxManager, blockManager, processor)
	// stopper.Add(blockManager)

	// processBlocksThread := threads.NewThread("Process Blocks", blockManager.Run)
	// processBlocksThread.SetWait(&wait)
	// processBlocksComplete := processBlocksThread.GetCompleteChannel()
	// stopper.Add(processBlocksThread)

	// ---------------------------------------------------------------------------------------------
	// Periodic

	saveThread, saveComplete := threads.NewPeriodicThreadComplete("Save",
		func(ctx context.Context) error {
			if err := headers.Clean(ctx); err != nil {
				return errors.Wrap(err, "clean headers")
			}
			if err := peers.Save(ctx); err != nil {
				return errors.Wrap(err, "save peers")
			}
			return nil
		}, 30*time.Minute, &wait)
	stopper.Add(saveThread)

	previousTime := time.Now()
	cleanTxsThread, cleanTxsComplete := threads.NewPeriodicThreadComplete("Clean Txs",
		func(ctx context.Context) error {
			if err := txManager.Clean(ctx, previousTime); err != nil {
				return errors.Wrap(err, "clean tx manager")
			}
			previousTime = time.Now()
			return nil
		}, 5*time.Minute, &wait)
	stopper.Add(cleanTxsThread)

	// ---------------------------------------------------------------------------------------------
	// Shutdown

	// Make a channel to listen for an interrupt or terminate signal from the OS. Use a buffered
	// channel because the signal package requires it.
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM)

	managerThread.Start(ctx)
	// processBlocksThread.Start(ctx)
	saveThread.Start(ctx)
	cleanTxsThread.Start(ctx)
	processTxThread.Start(ctx)

	// Blocking main and waiting for shutdown.
	select {
	case <-managerComplete:
		logger.Warn(ctx, "Finished: Manager")

	case <-saveComplete:
		logger.Warn(ctx, "Finished: Save")

	case <-cleanTxsComplete:
		logger.Warn(ctx, "Finished: Clean Txs")

	case <-processTxComplete:
		logger.Warn(ctx, "Finished: Process Txs")

	// case <-processBlocksComplete:
	// logger.Warn(ctx, "Finished: Process Blocks")

	case <-osSignals:
		logger.Info(ctx, "Shutdown requested")
	}

	// Stop remaining threads
	stopper.Stop(ctx)

	// Block until goroutines finish
	waitWarning := logger.NewWaitingWarning(ctx, 3*time.Second, "Shutdown")
	wait.Wait()
	waitWarning.Cancel()

	if err := headers.Save(ctx); err != nil {
		logger.Error(ctx, "Failed to save headers : %s", err)
	}
	if err := peers.Save(ctx); err != nil {
		logger.Error(ctx, "Failed to save peers : %s", err)
	}

	if err := threads.CombineErrors(
		managerThread.Error(),
		saveThread.Error(),
		cleanTxsThread.Error(),
	); err != nil {
		logger.Error(ctx, "Failed : %s", err)
	}
}
