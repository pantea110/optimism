package fault

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ethereum-optimism/optimism/op-bindings/bindings"
	"github.com/ethereum-optimism/optimism/op-challenger/config"
	"github.com/ethereum-optimism/optimism/op-challenger/fault/scheduler"
	"github.com/ethereum-optimism/optimism/op-challenger/fault/types"
	"github.com/ethereum-optimism/optimism/op-challenger/metrics"
	"github.com/ethereum-optimism/optimism/op-challenger/version"
	"github.com/ethereum-optimism/optimism/op-service/client"
	"github.com/ethereum-optimism/optimism/op-service/clock"
	oppprof "github.com/ethereum-optimism/optimism/op-service/pprof"
	"github.com/ethereum-optimism/optimism/op-service/txmgr"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
)

type Loader interface {
	FetchAbsolutePrestateHash(ctx context.Context) ([]byte, error)
}

type Service struct {
	logger  log.Logger
	metrics metrics.Metricer
	monitor *gameMonitor
	sched   *scheduler.Scheduler
}

// NewService creates a new Service.
func NewService(ctx context.Context, logger log.Logger, cfg *config.Config) (*Service, error) {
	cl := clock.SystemClock
	m := metrics.NewMetrics()
	txMgr, err := txmgr.NewSimpleTxManager("challenger", logger, &m.TxMetrics, cfg.TxMgrConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create the transaction manager: %w", err)
	}

	client, err := client.DialEthClientWithTimeout(client.DefaultDialTimeout, logger, cfg.L1EthRpc)
	if err != nil {
		return nil, fmt.Errorf("failed to dial L1: %w", err)
	}

	pprofConfig := cfg.PprofConfig
	if pprofConfig.Enabled {
		logger.Info("starting pprof", "addr", pprofConfig.ListenAddr, "port", pprofConfig.ListenPort)
		go func() {
			if err := oppprof.ListenAndServe(ctx, pprofConfig.ListenAddr, pprofConfig.ListenPort); err != nil {
				logger.Error("error starting pprof", "err", err)
			}
		}()
	}

	metricsCfg := cfg.MetricsConfig
	if metricsCfg.Enabled {
		logger.Info("starting metrics server", "addr", metricsCfg.ListenAddr, "port", metricsCfg.ListenPort)
		go func() {
			if err := m.Serve(ctx, metricsCfg.ListenAddr, metricsCfg.ListenPort); err != nil {
				logger.Error("error starting metrics server", "err", err)
			}
		}()
		m.StartBalanceMetrics(ctx, logger, client, txMgr.From())
	}

	factory, err := bindings.NewDisputeGameFactory(cfg.GameFactoryAddress, client)
	if err != nil {
		return nil, fmt.Errorf("failed to bind the fault dispute game factory contract: %w", err)
	}
	loader := NewGameLoader(factory)

	disk := newDiskManager(cfg.Datadir)
	sched := scheduler.NewScheduler(
		logger,
		disk,
		cfg.MaxConcurrency,
		func(addr common.Address, dir string) (scheduler.GamePlayer, error) {
			return NewGamePlayer(ctx, logger, cfg, dir, addr, txMgr, client)
		})

	monitor := newGameMonitor(logger, cl, loader, sched, cfg.GameWindow, client.BlockNumber, cfg.GameAllowlist)

	m.RecordInfo(version.SimpleWithMeta)
	m.RecordUp()

	return &Service{
		logger:  logger,
		metrics: m,
		monitor: monitor,
		sched:   sched,
	}, nil
}

// ValidateAbsolutePrestate validates the absolute prestate of the fault game.
func ValidateAbsolutePrestate(ctx context.Context, trace types.TraceProvider, loader Loader) error {
	providerPrestate, err := trace.AbsolutePreState(ctx)
	if err != nil {
		return fmt.Errorf("failed to get the trace provider's absolute prestate: %w", err)
	}
	providerPrestateHash := crypto.Keccak256(providerPrestate)
	onchainPrestate, err := loader.FetchAbsolutePrestateHash(ctx)
	if err != nil {
		return fmt.Errorf("failed to get the onchain absolute prestate: %w", err)
	}
	if !bytes.Equal(providerPrestateHash, onchainPrestate) {
		return fmt.Errorf("trace provider's absolute prestate does not match onchain absolute prestate")
	}
	return nil
}

// MonitorGame monitors the fault dispute game and attempts to progress it.
func (s *Service) MonitorGame(ctx context.Context) error {
	s.sched.Start(ctx)
	defer s.sched.Close()
	return s.monitor.MonitorGames(ctx)
}
