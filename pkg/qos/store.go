/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package qos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrPolicyNotFound is returned by a PolicyStore when the volume has no policy.
var ErrPolicyNotFound = errors.New("qos policy not found")

// PolicyStore keeps the policy snapshot of every volume. The standalone qosd
// service persists it in Redis; tests can use an in-memory implementation.
type PolicyStore interface {
	Get(ctx context.Context, volume string) (*Policy, error)
	Put(ctx context.Context, volume string, p *Policy) (*Policy, error)
	Delete(ctx context.Context, volume string) error
	List(ctx context.Context, tenant string) ([]*Policy, error)
	Close() error
}

const (
	redisPolicyPrefix  = "jfs:qos:policy:"
	redisVersionPrefix = "jfs:qos:version:"
	redisVolumesKey    = "jfs:qos:volumes"
	redisTenantPrefix  = "jfs:qos:tenant:"
)

// RedisStore stores one JSON blob per volume:
//
//	jfs:qos:policy:<volume>   the policy snapshot
//	jfs:qos:version:<volume>  monotonically increasing version counter
//	jfs:qos:volumes           set of every volume that has a policy
//	jfs:qos:tenant:<tenant>   set of volumes owned by one tenant
//
// The version counter is incremented by the service on every mutation, so a
// mount can tell whether its cached snapshot is current (and the version is
// visible in its log lines).
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore connects to Redis. addr accepts either a redis:// URL or a
// plain "host:port[/db]" address.
func NewRedisStore(addr string) (*RedisStore, error) {
	opt, err := parseRedisAddr(addr)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect redis %s: %w", addr, err)
	}
	return &RedisStore{client: client}, nil
}

func parseRedisAddr(addr string) (*redis.Options, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, errors.New("redis address is required")
	}
	if strings.Contains(addr, "://") {
		return redis.ParseURL(addr)
	}
	db := 0
	if i := strings.LastIndex(addr, "/"); i > strings.LastIndex(addr, ":") {
		n, err := strconv.Atoi(addr[i+1:])
		if err != nil {
			return nil, fmt.Errorf("invalid redis db in %q", addr)
		}
		db = n
		addr = addr[:i]
	}
	return &redis.Options{Addr: addr, DB: db}, nil
}

// Get returns the policy of one volume, or ErrPolicyNotFound.
func (s *RedisStore) Get(ctx context.Context, volume string) (*Policy, error) {
	data, err := s.client.Get(ctx, redisPolicyPrefix+volume).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrPolicyNotFound
	}
	if err != nil {
		return nil, err
	}
	p, err := ParsePolicy(data)
	if err != nil {
		return nil, err
	}
	p.Volume = volume
	return p, nil
}

// Put stores the policy, assigns the next version and updates the tenant and
// volume indexes. The rules keep their paths: the mount resolves them to
// inodes against its own namespace.
func (s *RedisStore) Put(ctx context.Context, volume string, p *Policy) (*Policy, error) {
	if volume == "" {
		return nil, errors.New("volume is required")
	}
	old, err := s.Get(ctx, volume)
	if err != nil && !errors.Is(err, ErrPolicyNotFound) {
		return nil, err
	}
	if err := p.init(); err != nil {
		return nil, err
	}
	version, err := s.client.Incr(ctx, redisVersionPrefix+volume).Result()
	if err != nil {
		return nil, err
	}
	p.Version = uint64(version)
	p.Volume = volume
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	pipe := s.client.TxPipeline()
	pipe.Set(ctx, redisPolicyPrefix+volume, data, 0)
	pipe.SAdd(ctx, redisVolumesKey, volume)
	pipe.SAdd(ctx, redisTenantPrefix+p.Tenant, volume)
	if old != nil && old.Tenant != "" && old.Tenant != p.Tenant {
		pipe.SRem(ctx, redisTenantPrefix+old.Tenant, volume)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// Delete removes the policy of a volume, or returns ErrPolicyNotFound.
func (s *RedisStore) Delete(ctx context.Context, volume string) error {
	old, err := s.Get(ctx, volume)
	if err != nil {
		return err
	}
	pipe := s.client.TxPipeline()
	pipe.Del(ctx, redisPolicyPrefix+volume, redisVersionPrefix+volume)
	pipe.SRem(ctx, redisVolumesKey, volume)
	if old.Tenant != "" {
		pipe.SRem(ctx, redisTenantPrefix+old.Tenant, volume)
	}
	_, err = pipe.Exec(ctx)
	return err
}

// List returns the policies of one tenant, or of every volume when tenant is
// empty. Volumes whose blob disappeared concurrently are skipped.
func (s *RedisStore) List(ctx context.Context, tenant string) ([]*Policy, error) {
	key := redisVolumesKey
	if tenant != "" {
		key = redisTenantPrefix + tenant
	}
	volumes, err := s.client.SMembers(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*Policy, 0, len(volumes))
	for _, volume := range volumes {
		p, err := s.Get(ctx, volume)
		if errors.Is(err, ErrPolicyNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Close releases the Redis connection.
func (s *RedisStore) Close() error { return s.client.Close() }
