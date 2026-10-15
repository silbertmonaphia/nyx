package cache

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

const (
	redisImage       = "redis:7-alpine"
	noDaemonSkipLine = "Docker daemon not reachable; skipping real-Redis integration tests"
)

var (
	integrationRedis *redis.RedisContainer
	redisAddr        string
)

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	switch {
	case os.Getenv("SKIP_CONTAINERS") == "true":
		fmt.Println("Skipping container-based integration tests")
	case dockerProviderHealth(ctx) != nil:
		fmt.Println(noDaemonSkipLine)
	default:
		addr, container, err := startRedisContainer(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Failed to start Redis container:", err)
			os.Exit(1)
		}
		integrationRedis, redisAddr = container, addr
	}

	code := m.Run()

	if integrationRedis != nil {
		_ = integrationRedis.Terminate(context.Background())
	}

	os.Exit(code)
}

func dockerProviderHealth(ctx context.Context) error {
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return err
	}
	defer func() { _ = provider.Close() }()

	return provider.Health(ctx)
}

func startRedisContainer(ctx context.Context) (string, *redis.RedisContainer, error) {
	hostPort, err := reserveHostPort()
	if err != nil {
		return "", nil, err
	}
	container, err := runRedis(ctx, hostPort)
	if err != nil {
		return "", nil, err
	}
	return "redis://127.0.0.1:" + hostPort, container, nil
}

func reserveHostPort() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = listener.Close() }()

	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), nil
}

func runRedis(ctx context.Context, hostPort string, opts ...testcontainers.ContainerCustomizer) (*redis.RedisContainer, error) {
	pinned := []testcontainers.ContainerCustomizer{
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.PortBindings = nat.PortMap{
				"6379/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: hostPort}},
			}
		}),
	}

	return redis.Run(ctx, redisImage, append(pinned, opts...)...)
}

func requireRedisAvailable(t *testing.T) {
	t.Helper()
	if redisAddr == "" {
		t.Skip("Redis container not available; set SKIP_CONTAINERS=false to run integration tests")
	}
}

func newIntegrationRedisCache(t *testing.T) Cache {
	t.Helper()
	requireRedisAvailable(t)
	c, err := NewRedis(redisAddr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRedisIntegrationReportsRedisMode(t *testing.T) {
	c := newIntegrationRedisCache(t)
	assert.Equal(t, "redis", c.Mode())
}

func TestRedisIntegrationTTLExpiryTiming(t *testing.T) {
	c := newIntegrationRedisCache(t)
	ctx := context.Background()

	require.NoError(t, c.Set(ctx, "integration:ttl", "ephemeral", 200*time.Millisecond))

	time.Sleep(250 * time.Millisecond)

	var got string
	hit, err := c.Get(ctx, "integration:ttl", &got)
	require.NoError(t, err)
	assert.False(t, hit, "an entry written with a 200ms TTL must read as the cache-miss sentinel after 250ms, not the stored value")
}

func TestRedisIntegrationDeletePrefixClearsFullNamespace(t *testing.T) {
	c := newIntegrationRedisCache(t)
	ctx := context.Background()

	var feedsKeys []string
	for i := 0; i < 250; i++ {
		key := fmt.Sprintf("feeds:q=a:p=%d:s=20", i)
		feedsKeys = append(feedsKeys, key)
		require.NoError(t, c.Set(ctx, key, "v", time.Minute), "seed %s", key)
	}
	require.NoError(t, c.Set(ctx, "other:1", "v", time.Minute))

	require.NoError(t, c.DeletePrefix(ctx, "feeds:"))

	var got string
	for _, key := range feedsKeys {
		hit, err := c.Get(ctx, key, &got)
		require.NoError(t, err)
		assert.False(t, hit, "%s must be removed by DeletePrefix(feeds:) — the batch flush spans more than one UNLINK", key)
	}

	hit, err := c.Get(ctx, "other:1", &got)
	require.NoError(t, err)
	assert.True(t, hit, "other:1 must survive DeletePrefix(feeds:)")
}

func TestRedisIntegrationNoevictionRefusesWritesWithoutPanic(t *testing.T) {
	requireRedisAvailable(t)
	ctx := context.Background()

	hostPort, err := reserveHostPort()
	require.NoError(t, err)

	container, err := runRedis(ctx, hostPort,
		testcontainers.WithCmdArgs("--maxmemory", "1mb", "--maxmemory-policy", "noeviction"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	c, err := NewRedis("redis://127.0.0.1:" + hostPort)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	payload := strings.Repeat("x", 256*1024)
	var refused error
	for i := 0; i < 64 && refused == nil; i++ {
		refused = c.Set(ctx, fmt.Sprintf("pressure:%d", i), payload, time.Minute)
	}
	require.Error(t, refused, "a 1MB noeviction instance must eventually refuse a write instead of evicting")

	assert.NotPanics(t, func() {
		var got string
		_, _ = c.Get(ctx, "pressure:0", &got)
		_ = c.DeletePrefix(ctx, "pressure:")
	}, "writes refused by noeviction must surface as errors the best-effort callers swallow, never as a panic")
}

func TestRedisIntegrationPingFailureFallsBackWithoutPanic(t *testing.T) {
	c := newIntegrationRedisCache(t)
	ctx := context.Background()

	require.NoError(t, c.Set(ctx, "integration:ping", "before", time.Minute))
	var got string
	hit, err := c.Get(ctx, "integration:ping", &got)
	require.NoError(t, err)
	require.True(t, hit, "precondition: the write must be readable before the container is stopped")

	require.NoError(t, integrationRedis.Stop(ctx, nil))
	t.Cleanup(func() { _ = integrationRedis.Start(ctx) })

	pingErr := c.Ping(ctx)
	assert.Error(t, pingErr, "Ping must surface the stopped backend so health probes see the outage")

	hit, getErr := c.Get(ctx, "integration:ping", &got)
	assert.False(t, hit, "Get against a stopped Redis must report the miss signal so callers fall through to the DB")
	assert.Error(t, getErr, "Get against a stopped Redis returns the transport error the caller is expected to swallow at Warn")

	assert.NotPanics(t, func() {
		_, _ = c.Get(ctx, "integration:ping", &got)
		_ = c.DeletePrefix(ctx, "integration:")
	}, "every cache call against a stopped Redis must return without panicking — the cache is best-effort")
}

func TestRedisIntegrationContainerRestartReconnectsTransparently(t *testing.T) {
	requireRedisAvailable(t)
	ctx := context.Background()

	require.NoError(t, integrationRedis.Stop(ctx, nil))
	require.NoError(t, integrationRedis.Start(ctx))

	var c Cache
	var constructErr error
	require.Eventually(t, func() bool {
		c, constructErr = NewRedis(redisAddr)
		return constructErr == nil
	}, 15*time.Second, 200*time.Millisecond, "the container must serve its mapped address again after Start, last NewRedis err=%v", constructErr)
	t.Cleanup(func() {
		if c != nil {
			_ = c.Close()
		}
	})

	var seeded string
	require.NoError(t, c.Set(ctx, "integration:restart", "before-outage", time.Minute))
	hit, err := c.Get(ctx, "integration:restart", &seeded)
	require.NoError(t, err)
	require.True(t, hit, "precondition: the client must be healthy before the outage")

	require.NoError(t, integrationRedis.Stop(ctx, nil))
	require.Error(t, c.Ping(ctx), "the stop must take the mapped address down so the reconnect below is real")

	require.NoError(t, integrationRedis.Start(ctx))

	var setErr error
	require.Eventually(t, func() bool {
		setErr = c.Set(ctx, "integration:restart", "after-restart", time.Minute)
		return setErr == nil
	}, 15*time.Second, 200*time.Millisecond, "the pre-existing client must reconnect to the restarted container on the same address, last Set err=%v", setErr)

	var got string
	hit, err = c.Get(ctx, "integration:restart", &got)
	require.NoError(t, err)
	assert.True(t, hit, "a write after the restart must be readable through the same client")
	assert.Equal(t, "after-restart", got)
}
