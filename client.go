package keel

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-kratos/kratos/v3/selector"
	"github.com/go-kratos/kratos/v3/selector/wrr"
	transgrpc "github.com/go-kratos/kratos/v3/transport/grpc"
	"google.golang.org/grpc"

	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/errs"
	"github.com/go-keel/keel/mw"
)

var selectorOnce sync.Once

// NewServiceClient creates a typed gRPC client for the service configured under client.<serviceName>, with the given
// ClientChain. A nil chain uses the recommended mw.ClientDefault(cfg); pass a custom chain only when the default does
// not fit. Returns the client, a cleanup that closes the connection, and any error.
func NewServiceClient[T any](
	cfg *kconfig.Config,
	serviceName string,
	newClient func(cc grpc.ClientConnInterface) T,
	chain *mw.ClientChain,
) (T, func(), error) {
	var zero T
	if cfg == nil || cfg.Config == nil || serviceName == "" || newClient == nil {
		return zero, nil, errs.Rules.New("client.invalid-params", "NewServiceClient was called with a nil config, an empty service name or a nil newClient", "pass the config from kconfig.NewConfig, the service name as it appears under `client` in the config, and the generated New<Service>Client function")
	}
	// Weighted Round Robin (the Kratos client's built-in default algorithm). Kratos reads the balancer from a global;
	// it is set on the first client built, not at import, so importing keel does not change other kratos clients.
	selectorOnce.Do(func() {
		selector.SetGlobalSelector(wrr.NewBuilder())
	})
	if chain == nil {
		chain = mw.ClientDefault(cfg)
	}
	if err := chain.Err(); err != nil {
		return zero, nil, errs.While("NewServiceClient: build middleware chain", err)
	}

	c := cfg.Client[serviceName]
	if c == nil {
		return zero, nil, errs.Rules.New("client.config-missing", fmt.Sprintf("NewServiceClient found no config for service %q", serviceName), fmt.Sprintf("add `client: {%s: {endpoint: ...}}` to the config or fix the service name", serviceName))
	}

	opts := []transgrpc.ClientOption{
		transgrpc.WithEndpoint(c.Endpoint),
		transgrpc.WithMiddleware(chain.Build()...),
	}
	if c.Timeout != nil {
		// Default per-RPC timeout. The connection itself is lazy (v3 uses grpc.NewClient underneath): construction
		// does not dial or block startup, and a first connection failure surfaces at RPC call time.
		opts = append(opts, transgrpc.WithTimeout(c.Timeout.AsDuration()))
	}

	conn, err := transgrpc.NewClient(context.Background(), opts...)
	if err != nil {
		return zero, nil, err
	}

	return newClient(conn), func() { _ = conn.Close() }, nil
}
