/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package redis

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"

	"github.com/dapr/components-contrib/configuration"
	"github.com/dapr/components-contrib/configuration/redis/internal"
	"github.com/dapr/kit/logger"
)

const (
	connectedSlavesReplicas   = "connected_slaves:"
	infoReplicationDelimiter  = "\r\n"
	host                      = "redisHost"
	password                  = "redisPassword"
	enableTLS                 = "enableTLS"
	maxRetries                = "maxRetries"
	maxRetryBackoff           = "maxRetryBackoff"
	failover                  = "failover"
	sentinelMasterName        = "sentinelMasterName"
	redisDB                   = "redisDB"
	defaultBase               = 10
	defaultBitSize            = 0
	defaultDB                 = 0
	defaultMaxRetries         = 3
	defaultMaxRetryBackoff    = time.Second * 2
	defaultEnableTLS          = false
	redisWrongTypeIdentifyStr = "WRONGTYPE"
)

// ConfigurationStore is a Redis configuration store.
type ConfigurationStore struct {
	client               redis.UniversalClient
	json                 jsoniter.API
	metadata             metadata
	replicas             int
	subscribeStopChanMap sync.Map

	logger logger.Logger
}

// NewRedisConfigurationStore returns a new redis state store.
func NewRedisConfigurationStore(logger logger.Logger) configuration.Store {
	s := &ConfigurationStore{
		json:   jsoniter.ConfigFastest,
		logger: logger,
	}

	return s
}

func parseRedisMetadata(meta configuration.Metadata) (metadata, error) {
	m := metadata{}

	if val, ok := meta.Properties[host]; ok && val != "" {
		m.Host = val
	} else {
		return m, errors.New("redis store error: missing host address")
	}

	if val, ok := meta.Properties[password]; ok && val != "" {
		m.Password = val
	}

	m.EnableTLS = defaultEnableTLS
	if val, ok := meta.Properties[enableTLS]; ok && val != "" {
		tls, err := strconv.ParseBool(val)
		if err != nil {
			return m, fmt.Errorf("redis store error: can't parse enableTLS field: %s", err)
		}
		m.EnableTLS = tls
	}

	m.MaxRetries = defaultMaxRetries
	if val, ok := meta.Properties[maxRetries]; ok && val != "" {
		parsedVal, err := strconv.ParseInt(val, defaultBase, defaultBitSize)
		if err != nil {
			return m, fmt.Errorf("redis store error: can't parse maxRetries field: %s", err)
		}
		m.MaxRetries = int(parsedVal)
	}

	m.MaxRetryBackoff = defaultMaxRetryBackoff
	if val, ok := meta.Properties[maxRetryBackoff]; ok && val != "" {
		parsedVal, err := strconv.ParseInt(val, defaultBase, defaultBitSize)
		if err != nil {
			return m, fmt.Errorf("redis store error: can't parse maxRetryBackoff field: %s", err)
		}
		m.MaxRetryBackoff = time.Duration(parsedVal)
	}

	if val, ok := meta.Properties[failover]; ok && val != "" {
		failover, err := strconv.ParseBool(val)
		if err != nil {
			return m, fmt.Errorf("redis store error: can't parse failover field: %s", err)
		}
		m.Failover = failover
	}

	// set the sentinelMasterName only with failover == true.
	if m.Failover {
		if val, ok := meta.Properties[sentinelMasterName]; ok && val != "" {
			m.SentinelMasterName = val
		} else {
			return m, errors.New("redis store error: missing sentinelMasterName")
		}
	}

	m.DB = defaultDB
	if val, ok := meta.Properties[redisDB]; ok && val != "" {
		parsedVal, err := strconv.ParseInt(val, defaultBase, defaultBitSize)
		if err != nil {
			return m, fmt.Errorf("redis store error: can't parse redisDB field from metadata: %w", err)
		}
		m.DB = int(parsedVal)
	}

	return m, nil
}

// Init does metadata and connection parsing.
func (r *ConfigurationStore) Init(ctx context.Context, metadata configuration.Metadata) error {
	m, err := parseRedisMetadata(metadata)
	if err != nil {
		return err
	}
	r.metadata = m

	if r.metadata.Failover {
		r.client = r.newFailoverClient(m)
	} else {
		r.client = r.newClient(m)
	}

	if _, err = r.client.Ping(ctx).Result(); err != nil {
		return fmt.Errorf("redis store: error connecting to redis at %s: %s", m.Host, err)
	}

	r.replicas, err = r.getConnectedSlaves(ctx)

	return err
}

func (r *ConfigurationStore) newClient(m metadata) *redis.Client {
	opts := &redis.Options{
		Addr:            m.Host,
		Password:        m.Password,
		DB:              m.DB,
		MaxRetries:      m.MaxRetries,
		MaxRetryBackoff: m.MaxRetryBackoff,
	}

	// tell the linter to skip a check here.
	/* #nosec */
	if m.EnableTLS {
		opts.TLSConfig = &tls.Config{
			InsecureSkipVerify: m.EnableTLS,
		}
	}

	return redis.NewClient(opts)
}

func (r *ConfigurationStore) newFailoverClient(m metadata) *redis.Client {
	opts := &redis.FailoverOptions{
		MasterName:      r.metadata.SentinelMasterName,
		SentinelAddrs:   []string{r.metadata.Host},
		DB:              m.DB,
		MaxRetries:      m.MaxRetries,
		MaxRetryBackoff: m.MaxRetryBackoff,
	}

	/* #nosec */
	if m.EnableTLS {
		opts.TLSConfig = &tls.Config{
			InsecureSkipVerify: m.EnableTLS,
		}
	}

	return redis.NewFailoverClient(opts)
}

func (r *ConfigurationStore) getConnectedSlaves(ctx context.Context) (int, error) {
	res, err := r.client.Do(ctx, "INFO", "replication").Result()
	if err != nil {
		return 0, err
	}

	// Response example: https://redis.io/commands/info#return-value
	// # Replication\r\nrole:master\r\nconnected_slaves:1\r\n
	s, _ := strconv.Unquote(fmt.Sprintf("%q", res))
	if len(s) == 0 {
		return 0, nil
	}

	return r.parseConnectedSlaves(s), nil
}

func (r *ConfigurationStore) parseConnectedSlaves(res string) int {
	infos := strings.Split(res, infoReplicationDelimiter)
	for _, info := range infos {
		if strings.Contains(info, connectedSlavesReplicas) {
			parsedReplicas, _ := strconv.ParseUint(info[len(connectedSlavesReplicas):], 10, 32)
			return int(parsedReplicas)
		}
	}

	return 0
}

func (r *ConfigurationStore) Get(ctx context.Context, req *configuration.GetRequest) (*configuration.GetResponse, error) {
	keys := req.Keys
	var err error
	if len(keys) == 0 {
		if keys, err = r.client.Keys(ctx, "*").Result(); err != nil {
			r.logger.Errorf("failed to all keys, error is %s", err)
		}
	}

	items := make(map[string]*configuration.Item, len(keys))

	// query by keys
	for _, redisKey := range keys {
		item := &configuration.Item{
			Metadata: map[string]string{},
		}

		redisValue, err := r.client.Get(ctx, redisKey).Result()
		if err != nil {
			if err == redis.Nil {
				r.logger.Warnf("redis key %s does not exist, ignore it\n", redisKey)
				continue
			}
			if strings.Contains(err.Error(), redisWrongTypeIdentifyStr) {
				r.logger.Warnf("redis key %s 's type is not supported, ignore it\n", redisKey)
				continue
			}
			return &configuration.GetResponse{}, fmt.Errorf("fail to get configuration for redis key=%s, error is %s", redisKey, err)
		}
		val, version := internal.GetRedisValueAndVersion(redisValue)
		item.Version = version
		item.Value = val

		if item.Value != "" {
			items[redisKey] = item
		}
	}

	return &configuration.GetResponse{
		Items: items,
	}, nil
}

func (r *ConfigurationStore) Subscribe(ctx context.Context, req *configuration.SubscribeRequest, handler configuration.UpdateHandler) (string, error) {
	subscribeID := uuid.New().String()
	keyStopChanMap := make(map[string]chan struct{})
	if len(req.Keys) == 0 {
		// subscribe all keys
		stop := make(chan struct{})
		allKeysChannel := internal.GetRedisChannelFromKey("*", r.metadata.DB)
		keyStopChanMap[allKeysChannel] = stop
		go r.doSubscribe(ctx, req, handler, allKeysChannel, subscribeID, stop)
		r.subscribeStopChanMap.Store(subscribeID, keyStopChanMap)
		return subscribeID, nil
	}

	for _, k := range req.Keys {
		// subscribe single key
		stop := make(chan struct{})
		redisChannel := internal.GetRedisChannelFromKey(k, r.metadata.DB)
		keyStopChanMap[redisChannel] = stop
		go r.doSubscribe(ctx, req, handler, redisChannel, subscribeID, stop)
	}
	r.subscribeStopChanMap.Store(subscribeID, keyStopChanMap)
	return subscribeID, nil
}

func (r *ConfigurationStore) Unsubscribe(ctx context.Context, req *configuration.UnsubscribeRequest) error {
	if keyStopChanMap, ok := r.subscribeStopChanMap.Load(req.ID); ok {
		// already exist subscription
		for _, stop := range keyStopChanMap.(map[string]chan struct{}) {
			close(stop)
		}
		r.subscribeStopChanMap.Delete(req.ID)
		return nil
	}
	return fmt.Errorf("subscription with id %s does not exist", req.ID)
}

func (r *ConfigurationStore) doSubscribe(ctx context.Context, req *configuration.SubscribeRequest, handler configuration.UpdateHandler, redisChannel4revision string, id string, stop chan struct{}) {
	// enable notify-keyspace-events by redis Set command
	// only subscribe to generic and string keyspace events
	r.client.ConfigSet(ctx, "notify-keyspace-events", "Kg$xe")
	var p *redis.PubSub
	allKeysChannel := internal.GetRedisChannelFromKey("*", r.metadata.DB)
	if redisChannel4revision == allKeysChannel {
		p = r.client.PSubscribe(ctx, redisChannel4revision)
	} else {
		p = r.client.Subscribe(ctx, redisChannel4revision)
	}
	defer p.Close()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case msg := <-p.Channel():
			r.handleSubscribedChange(ctx, req, handler, msg, id)
		}
	}
}

func (r *ConfigurationStore) handleSubscribedChange(ctx context.Context, req *configuration.SubscribeRequest, handler configuration.UpdateHandler, msg *redis.Message, id string) {
	targetKey, err := internal.ParseRedisKeyFromChannel(msg.Channel, r.metadata.DB)
	if err != nil {
		r.logger.Errorf("parse redis key failed: %s", err)
		return
	}

	var items map[string]*configuration.Item

	// get all keys if only one is changed
	getResponse, errGet := r.Get(ctx, &configuration.GetRequest{
		Metadata: req.Metadata,
		Keys:     []string{targetKey},
	})
	if errGet != nil {
		r.logger.Errorf("get response from redis failed: %s", err)
		return
	}
	items = getResponse.Items
	if len(items) == 0 {
		items = map[string]*configuration.Item{
			targetKey: {},
		}
	}

	e := &configuration.UpdateEvent{
		Items: items,
		ID:    id,
	}
	err = handler(ctx, e)
	if err != nil {
		r.logger.Errorf("fail to call handler to notify event for configuration update subscribe: %s", err)
	}
}
