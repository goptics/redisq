package redisq

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openInstances returns queues in namespace ns with its instance keys cleared
// before and after the test.
func openInstances(t *testing.T, ns string, opts ...Option) *queues {
	t.Helper()
	qs := New(getTestRedisURL(), append([]Option{WithNamespace(ns)}, opts...)...).(*queues)
	k := qs.instancesKey()
	require.NoError(t, qs.client.Del(context.Background(), k, k+":beats").Err())
	t.Cleanup(func() {
		require.NoError(t, qs.client.Del(context.Background(), k, k+":beats").Err())
		_ = qs.Close()
	})
	return qs
}

func redisNowMS(t *testing.T, c *redis.Client) int64 {
	t.Helper()
	now, err := c.Time(t.Context()).Result()
	require.NoError(t, err)
	return now.UnixMilli()
}

// A report is read back with its time from the Redis clock, stores no
// beat_at or lost, lets both keys expire, and goes on remove.
func TestInstanceRegistry(t *testing.T) {
	qs := openInstances(t, "test_instances_registry")
	ctx := t.Context()
	k := qs.instancesKey()
	rec := contract.InstanceRecord{
		ID: "pod-a", Host: "pod-a", PID: 7, Version: "v1.11.0", StartedAt: 1, IntervalMS: 5000,
		BeatAt: 99, Lost: true, // ignored: storage never keeps them
		Workers: []contract.InstanceWorker{{Name: "emails", Status: "Running", Concurrency: 4, QueueIDs: []string{"redisq:prio:{emails}"}}},
	}

	before := redisNowMS(t, qs.client)
	require.NoError(t, qs.ReportInstance(ctx, rec))

	raw, err := qs.client.HGet(ctx, k, "pod-a").Result()
	require.NoError(t, err)
	assert.NotContains(t, raw, "beat_at")
	assert.NotContains(t, raw, "lost")
	for _, key := range []string{k, k + ":beats"} {
		ttl, err := qs.client.PTTL(ctx, key).Result()
		require.NoError(t, err)
		assert.True(t, ttl > 0 && ttl <= pruneAfterMS*time.Millisecond, "%s ttl %v", key, ttl)
	}

	got, err := qs.Instances(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.GreaterOrEqual(t, got[0].BeatAt, before)
	assert.False(t, got[0].Lost)
	want := rec
	want.BeatAt, want.Lost = got[0].BeatAt, false
	assert.Equal(t, want, got[0])

	require.NoError(t, qs.RemoveInstance(ctx, "pod-a"))
	got, err = qs.Instances(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)

	assert.Error(t, qs.ReportInstance(ctx, contract.InstanceRecord{}), "empty ID")
}

// Readers mark a record lost after 3 intervals and skip one older than the
// prune age, one without a record, or one that does not parse; the next
// report prunes the old one.
func TestInstancesLostAndStale(t *testing.T) {
	qs := openInstances(t, "test_instances_stale")
	ctx := t.Context()
	k := qs.instancesKey()
	now := redisNowMS(t, qs.client)
	rec := func(id string) string { return `{"id":"` + id + `","interval_ms":5000,"workers":[],"extra":1}` }

	require.NoError(t, qs.client.HSet(ctx, k, "lost", rec("lost"), "ancient", rec("ancient"), "bad", "not json").Err())
	require.NoError(t, qs.client.ZAdd(ctx, k+":beats",
		redis.Z{Score: float64(now - 20_000), Member: "lost"},
		redis.Z{Score: float64(now - 400_000), Member: "ancient"},
		redis.Z{Score: float64(now), Member: "bad"},
		redis.Z{Score: float64(now), Member: "no-record"},
	).Err())

	got, err := qs.Instances(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "lost", got[0].ID)
	assert.True(t, got[0].Lost)
	assert.Equal(t, now-20_000, got[0].BeatAt)

	require.NoError(t, qs.ReportInstance(ctx, contract.InstanceRecord{ID: "live", IntervalMS: 5000}))
	assert.False(t, qs.client.HExists(ctx, k, "ancient").Val(), "pruned")
	_, err = qs.client.ZScore(ctx, k+":beats", "ancient").Result()
	assert.ErrorIs(t, err, redis.Nil, "pruned")
	assert.True(t, qs.client.HExists(ctx, k, "lost").Val(), "lost but younger than 5 min: kept")
}

// WithInstanceReports reports from New until Close, which removes the
// record; a bad interval makes New panic.
func TestWithInstanceReports(t *testing.T) {
	contract.SetInstanceSource(func() contract.InstanceRecord {
		return contract.InstanceRecord{ID: "pod-reports", Workers: []contract.InstanceWorker{}}
	})

	reader := openInstances(t, "test_instances_option")
	qs := New(getTestRedisURL(), WithNamespace("test_instances_option"), WithInstanceReports(time.Second)).(*queues)

	ids := func() []string {
		got, err := reader.Instances(context.Background())
		require.NoError(t, err)
		var out []string
		for _, r := range got {
			out = append(out, r.ID+"@"+strconv.FormatInt(r.IntervalMS, 10))
		}
		return out
	}
	assert.Eventually(t, func() bool { return len(ids()) == 1 }, 3*time.Second, 50*time.Millisecond)
	assert.Equal(t, []string{"pod-reports@1000"}, ids())

	require.NoError(t, qs.Close())
	assert.Empty(t, ids(), "Close removes the record")

	assert.Panics(t, func() { New(getTestRedisURL(), WithInstanceReports(500*time.Millisecond)) })
	assert.Panics(t, func() { New(getTestRedisURL(), WithInstanceReports(61*time.Second)) })
}

// On a closed client every registry call returns the client's error.
func TestInstanceRegistryClosedClient(t *testing.T) {
	qs := New(getTestRedisURL(), WithNamespace("test_instances_closed")).(*queues)
	require.NoError(t, qs.Close())
	ctx := t.Context()
	assert.Error(t, qs.ReportInstance(ctx, contract.InstanceRecord{ID: "x"}))
	assert.Error(t, qs.RemoveInstance(ctx, "x"))
	_, err := qs.Instances(ctx)
	assert.Error(t, err)
}
