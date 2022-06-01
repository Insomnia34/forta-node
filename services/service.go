package services

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/forta-network/forta-core-go/ens"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/forta-network/forta-node/config"
)

const (
	defaultServiceStartDelay = time.Minute * 10
)

const (
	GracefulShutdownSignal = syscall.SIGTERM
)

// Service is a service abstraction.
type Service interface {
	Start() error
	Stop() error
	Name() string
}

var sigc = make(chan os.Signal)

var execIDKey = struct{}{}

func ExecID(ctx context.Context) string {
	execID := ctx.Value(execIDKey)
	if execID == nil {
		panic("cannot get exec ID")
	}
	return execID.(string)
}

func initExecID(ctx context.Context) context.Context {
	execID, err := uuid.NewUUID()
	if err != nil {
		panic(err)
	}
	return context.WithValue(ctx, execIDKey, execID.String())
}

func setContracts(cfg *config.Config) error {
	es, err := ens.DialENSStoreAt(cfg.ENSConfig.JsonRpc.Url, cfg.ENSConfig.ContractAddress)
	if err != nil {
		return err
	}

	contracts, err := es.ResolveRegistryContracts()
	if err != nil {
		return err
	}

	if cfg.Registry.ContractAddress == "" {
		cfg.Registry.ContractAddress = contracts.Dispatch.Hex()
	}
	cfg.ScannerVersionContractAddress = contracts.ScannerRegistry.Hex()
	cfg.AgentRegistryContractAddress = contracts.AgentRegistry.Hex()
	return nil
}

// ContainerMainOpts contains some options.
type ContainerMainOpts struct {
	SkipENS bool
}

func ContainerMain(
	name string, getServices func(ctx context.Context, cfg config.Config) ([]Service, error),
	opts ContainerMainOpts,
) {
	logger := log.WithField("container", name)

	cfg, err := config.GetConfigForContainer()
	if err != nil {
		logger.WithError(err).Error("could not get config")
		return
	}

	if !opts.SkipENS {
		if err := setContracts(&cfg); err != nil {
			logger.WithError(err).Error("could not initialize contract addresses using config")
			return
		}
	}

	lvl, err := log.ParseLevel(cfg.Log.Level)
	if err != nil {
		logger.WithError(err).Error("could not initialize log level")
		return
	}
	log.SetLevel(lvl)
	log.SetFormatter(&log.JSONFormatter{})
	logger.Info("starting")
	defer logger.Info("exiting")

	ctx, cancel := InitMainContext()
	defer cancel()

	serviceList, err := getServices(ctx, cfg)
	if err != nil {
		logger.WithError(err).Error("could not initialize services")
		return
	}

	if err := StartServices(ctx, cancel, logger, serviceList); err != nil {
		logger.WithError(err).Error("failed to start services")
	}
}

var gracefulShutdown bool

// IsGracefulShutdown tells if we have reached a graceful shutdown condition.
func IsGracefulShutdown() bool {
	return gracefulShutdown
}

func InitMainContext() (context.Context, context.CancelFunc) {
	execIDCtx := initExecID(context.Background())
	ctx, cancel := context.WithCancel(execIDCtx)
	signal.Notify(sigc,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func() {
		sig := <-sigc
		log.Infof("received signal: %s", sig.String())
		gracefulShutdown = sig == GracefulShutdownSignal
		cancel()
	}()
	return ctx, cancel
}

// InterruptMainContext interrupts by sending a fake interrup signal from within runtime.
func InterruptMainContext() {
	select {
	case sigc <- syscall.SIGINT:
	default:
	}
}

// StartServices kicks off all services.
func StartServices(ctx context.Context, cancelMainCtx context.CancelFunc, logger *log.Entry, services []Service) error {
	// each service should be able to start successfully within reasonable time
	for _, service := range services {
		serviceStartedCtx, serviceStarted := context.WithCancel(context.Background())
		defer serviceStarted()

		logger := logger.WithField("service", service.Name())

		go func() {
			if err := service.Start(); err != nil {
				logger.WithError(err).Error("failed to start service")
				cancelMainCtx()
				return
			}
			serviceStarted()
		}()

		select {
		case <-time.After(defaultServiceStartDelay):
			logger.Error("took too long to start service")
			cancelMainCtx()
			break
		case <-serviceStartedCtx.Done():
			// ok - do nothing
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	<-ctx.Done()
	logger.WithError(ctx.Err()).Info("context is done")

	// stop all services
	for _, service := range services {
		err := service.Stop()
		logger.WithError(err).WithField("service", service.Name()).Info("stopped")
	}

	return nil
}
