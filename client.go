package keel

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-kratos/kratos/v3/selector"
	"github.com/go-kratos/kratos/v3/selector/wrr"
	transgrpc "github.com/go-kratos/kratos/v3/transport/grpc"
	"google.golang.org/grpc"

	"github.com/go-keel/keel/internal/errs"
	"github.com/go-keel/keel/mw"
)

var selectorOnce sync.Once

type ServiceClientFactory struct {
	c *Config
}

func NewServiceClientFactory(c *Config) *ServiceClientFactory {
	// Weighted Round Robin (the Kratos client's built-in default algorithm).
	// Set at explicit construction so an import side effect does not affect other kratos clients in the same process.
	selectorOnce.Do(func() {
		selector.SetGlobalSelector(wrr.NewBuilder())
	})
	return &ServiceClientFactory{c: c}
}

// Config exposes the underlying config for building a ClientChain.
func (f *ServiceClientFactory) Config() *Config { return f.c }

// NewServiceClient creates a typed gRPC client with the given ClientChain.
// A nil chain uses the recommended mw.ClientDefault. Pass a custom chain only
// when the default does not fit. Returns the client, a cleanup func, and any error.
func NewServiceClient[T any](
	factory *ServiceClientFactory,
	serviceName string,
	newClient func(cc grpc.ClientConnInterface) T,
	chain *mw.ClientChain,
) (T, func(), error) {
	var zero T
	if factory == nil || factory.c == nil || serviceName == "" || newClient == nil {
		return zero, nil, errs.Rules.New("client.invalid-params", "NewServiceClient was called with a nil factory, a factory without config, an empty service name or a nil newClient", "pass the factory from NewServiceClientFactory, the service name as it appears under `client` in the config, and the generated New<Service>Client function")
	}
	if chain == nil {
		chain = mw.ClientDefault(factory.c.Config)
	}
	if err := chain.Err(); err != nil {
		return zero, nil, errs.While("NewServiceClient: build middleware chain", err)
	}

	c := factory.c.Client[serviceName]
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
