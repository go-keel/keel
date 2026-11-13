package keel

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-kratos/kratos/v3"
	"github.com/go-kratos/kratos/v3/transport"

	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/appid"
	"github.com/go-keel/keel/internal/errs"
)

// App is an application instance.
type App struct {
	*kratos.App
}

// Server is a new type over kratos transport.Server. Everywhere Keel itself returns a server
// (api.NewHTTPServer/NewGRPCServer, knats.NewConsumerServer, worker.Server) and New's parameters present it
// under this name: Keel is a framework, and the type users write every day should be its own name, not the
// underlying kratos one. It is an interface over an interface with an identical method set, so assignment in
// either direction needs no explicit conversion; the reasons and rejected approaches are in ADR keel-0010.
type Server transport.Server

func NewAppFactory(config *Config) *AppFactory {
	return &AppFactory{config: config}
}

type AppFactory struct {
	config *Config
}

func (f *AppFactory) New(servers ...Server) (*App, error) {
	if len(servers) == 0 {
		return nil, errs.Rules.New("app.no-servers", "AppFactory.New was called with no transport servers", "pass at least one server, such as the one from api.NewHTTPServer or api.NewGRPCServer")
	}
	if f.config == nil || f.config.App == nil {
		return nil, errs.Rules.New("app.config-missing", "AppFactory has no app config (the config or its app section is nil)", "load the config with kconfig and make sure it has an `app` section")
	}

	appId := appid.InstanceID(f.config.App)

	// keel.Config is a named type of kconfig.Config (same underlying struct). The conversion lives in one place,
	// kconfig.Config.StopTimeout; asynq.Config.ShutdownTimeout inside kasynq.WorkerServer is converted through it
	// too, so the two places cannot each have their own default.
	st := (*kconfig.Config)(f.config).StopTimeout()

	// kratos.Server takes ...transport.Server. Server is a new type over it, so a single value needs no conversion
	// either way, but []Server and []transport.Server are different slice types (different element types make
	// different slice types), so the elements must be copied one by one. This is the only internal seam that
	// switches to kratos's own API; no other hand-off point, for users or inside the framework, needs this step.
	ts := make([]transport.Server, len(servers))
	for i, s := range servers {
		ts[i] = s
	}

	kOpts := []kratos.Option{
		kratos.ID(appId),
		kratos.Name(f.config.App.Name),
		kratos.Version(f.config.App.Version),
		// Logging stays out of the dependency graph: use the process default logger, set by SetupLogger at the start of assembly.
		kratos.Logger(slog.Default()),
		kratos.Server(ts...),
		// Every server's Stop(ctx) looks only at this one deadline; it is the single source of "how long to wait".
		kratos.StopTimeout(st),
	}

	return &App{
		App: kratos.New(kOpts...),
	}, nil
}

func (f *AppFactory) Config() *Config {
	return f.config
}

// Run starts the servers and blocks until they stop. The first SIGINT/SIGTERM/SIGQUIT starts kratos's graceful stop
// (every server gets up to app.stop_timeout); a second one exits the process at once with code 130, skipping the
// remaining stops and the cleanups, so a stop that hangs does not leave the developer waiting for the timeout.
func (a *App) Run() error {
	c := make(chan os.Signal, 2)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	stop := watchSecondSignal(c, os.Exit)
	defer func() {
		signal.Stop(c)
		stop()
	}()
	return a.App.Run()
}

// watchSecondSignal calls exit(130) when a second signal arrives on c; stop ends the watch.
func watchSecondSignal(c <-chan os.Signal, exit func(int)) (stop func()) {
	done := make(chan struct{})
	go func() {
		seen := 0
		for {
			select {
			case <-done:
				return
			case s := <-c:
				seen++
				if seen == 2 {
					slog.Warn("keel: second signal, exiting now without waiting for servers to stop or cleanups to run", "signal", s.String())
					exit(130)
					return
				}
			}
		}
	}()
	return func() { close(done) }
}
