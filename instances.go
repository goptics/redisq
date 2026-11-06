package redisq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/redis/go-redis/v9"
)

// pruneAfterMS is how old a report gets before it is pruned (spec
// redis-storage.md 8.3): more than 3 times the longest interval, 60s.
const pruneAfterMS = 300_000

// reportInstanceScript writes one Instance Record and its report time from
// the Redis clock, prunes up to 100 stale records, and lets both keys expire
// when no instance reports (spec redis-storage.md 8.3).
// KEYS: {instances}, {instances}:beats. ARGV: Instance ID, record JSON.
var reportInstanceScript = redis.NewScript(`redis.replicate_commands()
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('ZADD', KEYS[2], string.format('%.0f', now), ARGV[1])
local stale = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', '(' .. string.format('%.0f', now - 300000), 'LIMIT', 0, 100)
for _, id in ipairs(stale) do
	redis.call('HDEL', KEYS[1], id)
	redis.call('ZREM', KEYS[2], id)
end
redis.call('PEXPIRE', KEYS[1], 300000)
redis.call('PEXPIRE', KEYS[2], 300000)
return 1`)

// WithInstanceReports makes New report this process to the namespace's
// Instance Records at start and then every interval (1s to 60s), so
// observers such as vartop list it from Redis without calling it. Close
// stops the reports and removes the record. The record comes from varmq
// (worker names, status, concurrency, bound Queue IDs); without varmq in the
// binary nothing is written. Set varmq.InstanceID before New: it is read
// once, and an empty one means no reports. Turn it on for one client per
// namespace, as Close removes the process's shared record; Close waits up to
// 5s for that remove.
func WithInstanceReports(interval time.Duration) Option {
	return func(qs *queues) error {
		if interval < time.Second || interval > time.Minute {
			return fmt.Errorf("redisq: instance report interval %v must be from 1s to 60s", interval)
		}
		qs.reportEvery = interval
		return nil
	}
}

// startReports runs contract.ReportInstances until Close.
func (qs *queues) startReports() {
	ctx, cancel := context.WithCancel(context.Background())
	qs.stopReports = cancel
	qs.reportsDone = make(chan struct{})
	go func() {
		defer close(qs.reportsDone)
		_ = contract.ReportInstances(ctx, qs, qs.reportEvery) // ponytail: no error hook; an empty InstanceID just means no reports
	}()
}

// instancesKey is the namespace's Instance Record hash; ":beats" is its
// report-time set. The hash tag puts both in one cluster slot.
func (qs *queues) instancesKey() string { return qs.prefix() + "{instances}" }

// ReportInstance implements contract.InstanceRegistry. Owners are left
// out (spec allows it). ponytail: add them with job cancel, their only user.
func (qs *queues) ReportInstance(ctx context.Context, r contract.InstanceRecord) error {
	if r.ID == "" {
		return errors.New("redisq: instance record needs a non-empty ID")
	}
	r.BeatAt, r.Lost = 0, false // the report time lives in :beats, never in the record
	body, _ := json.Marshal(r)  // plain fields only: cannot fail
	k := qs.instancesKey()
	return reportInstanceScript.Run(ctx, qs.client, []string{k, k + ":beats"}, r.ID, body).Err()
}

// RemoveInstance implements contract.InstanceRegistry.
func (qs *queues) RemoveInstance(ctx context.Context, id string) error {
	k := qs.instancesKey()
	_, err := qs.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HDel(ctx, k, id)
		p.ZRem(ctx, k+":beats", id)
		return nil
	})
	return err
}

// Instances implements contract.InstanceRegistry: every record reported in
// the last 5 minutes, oldest report first, with BeatAt and Lost from the
// Redis clock (spec redis-storage.md 8.5). It writes nothing; an ACL user
// needs +time +zrange +hgetall +multi +exec on the {instances} keys.
func (qs *queues) Instances(ctx context.Context) ([]contract.InstanceRecord, error) {
	k := qs.instancesKey()
	var now *redis.TimeCmd
	var beats *redis.ZSliceCmd
	var recs *redis.MapStringStringCmd
	if _, err := qs.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		now = p.Time(ctx)
		beats = p.ZRangeWithScores(ctx, k+":beats", 0, -1)
		recs = p.HGetAll(ctx, k)
		return nil
	}); err != nil {
		return nil, err
	}

	nowMS := now.Val().UnixMilli()
	out := []contract.InstanceRecord{}
	for _, z := range beats.Val() {
		id, _ := z.Member.(string)
		raw, ok := recs.Val()[id]
		beat := int64(z.Score)
		if !ok || beat < nowMS-pruneAfterMS { // a remove or prune ran between writes, or due for pruning
			continue
		}
		var r contract.InstanceRecord
		if json.Unmarshal([]byte(raw), &r) != nil {
			continue
		}
		r.ID, r.BeatAt, r.Lost = id, beat, beat < nowMS-3*r.IntervalMS
		out = append(out, r)
	}
	return out, nil
}
