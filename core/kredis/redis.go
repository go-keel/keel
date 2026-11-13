package kredis

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"

	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/internal/errs"
)

// pingTimeout bounds the startup connectivity probe so that an unreachable redis does not block forever.
const pingTimeout = 5 * time.Second

type Redis struct {
	redis.UniversalClient
}

// NewRedis opens the "default" Redis client. The returned cleanup closes
// the client.
func NewRedis(config *kconfig.Config) (*Redis, func(), error) {
	return NewNamedRedis(config, "default")
}

func NewNamedRedis(config *kconfig.Config, name string) (*Redis, func(), error) {
	if len(config.Redis) == 0 {
		return nil, nil, errs.Rules.New("kredis.config-missing", "the config has no `redis` section", "add `redis: {default: {addr: [...]}}` to the config")
	}
	if name == "" {
		name = "default"
	}

	c := config.Redis[name]
	if c == nil {
		return nil, nil, errs.Rules.New("kredis.config-missing", fmt.Sprintf("the config has no redis.%s section", name), fmt.Sprintf("add `redis: {%s: {addr: [...]}}` to the config or fix the name passed to NewNamedRedis", name))
	}

	// UniversalClient decides between standalone / sentinel / cluster from MasterName and the number of addresses;
	// the rules are in Redis.addr in conf.proto. With PoolSize 0, go-redis uses its own default.
	rdb := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:            c.Addr,
		Username:         c.Username,
		Password:         c.Password,
		DB:               int(c.Db),
		ReadTimeout:      c.ReadTimeout.AsDuration(),
		WriteTimeout:     c.WriteTimeout.AsDuration(),
		MasterName:       c.MasterName,
		SentinelPassword: c.SentinelPassword,
		PoolSize:         int(c.PoolSize),
	})
	cleanup := func() { _ = rdb.Close() }

	// Take otel's global TracerProvider directly (a process facility), for the same reason as kdb.NewNamedDB.
	if err := redisotel.InstrumentTracing(rdb,
		redisotel.WithTracerProvider(otel.GetTracerProvider()),
	); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("kredis: otel instrument (name=%s): %w", name, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("kredis: ping %v (name=%s): %w", c.Addr, name, err)
	}

	return &Redis{UniversalClient: rdb}, cleanup, nil
}
