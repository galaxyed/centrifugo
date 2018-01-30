package engineredis

import (
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/centrifugal/centrifugo/lib/channel"
	"github.com/centrifugal/centrifugo/lib/engine"
	"github.com/centrifugal/centrifugo/lib/logger"
	"github.com/centrifugal/centrifugo/lib/node"
	"github.com/centrifugal/centrifugo/lib/proto"
	"github.com/centrifugal/centrifugo/lib/proto/controlproto"

	"github.com/FZambia/go-sentinel"
	"github.com/garyburd/redigo/redis"
)

const (
	// RedisSubscribeChannelSize is the size for the internal buffered channels RedisEngine
	// uses to synchronize subscribe/unsubscribe.
	RedisSubscribeChannelSize = 4096
	// RedisPubSubWorkerChannelSize sets buffer size of channel to which we send all
	// messages received from Redis PUB/SUB connection to process in separate goroutine.
	RedisPubSubWorkerChannelSize = 4096
	// RedisSubscribeBatchLimit is a maximum number of channels to include in a single subscribe
	// call. Redis documentation doesn't specify a maximum allowed but we think it probably makes
	// sense to keep a sane limit given how many subscriptions a single Centrifugo instance might
	// be handling.
	RedisSubscribeBatchLimit = 2048
	// RedisPublishChannelSize is the size for the internal buffered channel RedisEngine uses
	// to collect publish requests.
	RedisPublishChannelSize = 1024
	// RedisPublishBatchLimit is a maximum limit of publish requests one batched publish
	// operation can contain.
	RedisPublishBatchLimit = 2048
	// RedisDataBatchLimit limits amount of data operations combined in one pipeline.
	RedisDataBatchLimit = 8
	// RedisDataChannelSize is a buffer size of channel with data operation requests.
	RedisDataChannelSize = 256
)

type (
	// channelID is unique channel identificator in Redis.
	channelID string
)

const (
	// RedisControlChannelSuffix is a suffix for control channel.
	RedisControlChannelSuffix = ".control"
	// RedisPingChannelSuffix is a suffix for ping channel.
	RedisPingChannelSuffix = ".ping"
	// RedisClientChannelPrefix is a prefix before channel name for client messages.
	RedisClientChannelPrefix = ".client."
)

// RedisEngine uses Redis datastructures and PUB/SUB to manage Centrifugo logic.
// This engine allows to scale Centrifugo - you can run several Centrifugo instances
// connected to the same Redis and load balance clients between instances.
type RedisEngine struct {
	sync.RWMutex
	node     *node.Node
	config   *Config
	sharding bool
	shards   []*Shard
}

// Shard has everything to connect to Redis instance.
type Shard struct {
	sync.RWMutex
	node              *node.Node
	config            *ShardConfig
	pool              *redis.Pool
	subCh             chan subRequest
	pubCh             chan pubRequest
	dataCh            chan dataRequest
	pubScript         *redis.Script
	addPresenceScript *redis.Script
	remPresenceScript *redis.Script
	presenceScript    *redis.Script
	lpopManyScript    *redis.Script
	messagePrefix     string
}

// Config of Redis Engine.
type Config struct {
	Shards []*ShardConfig
}

// ShardConfig is struct with Redis Engine options.
type ShardConfig struct {
	// Host is Redis server host.
	Host string
	// Port is Redis server port.
	Port string
	// Password is password to use when connecting to Redis database. If empty then password not used.
	Password string
	// DB is Redis database number. If not set then database 0 used.
	DB int
	// MasterName is a name of Redis instance master Sentinel monitors.
	MasterName string
	// SentinelAddrs is a slice of Sentinel addresses.
	SentinelAddrs []string
	// PoolSize is a size of Redis connection pool.
	PoolSize int
	// Prefix to use before every channel name and key in Redis.
	Prefix string
	// PubSubNumWorkers sets how many PUB/SUB message processing workers will be started.
	// By default we start runtime.NumCPU() workers.
	PubSubNumWorkers int
	// ReadTimeout is a timeout on read operations. Note that at moment it should be greater
	// than node ping publish interval in order to prevent timing out Pubsub connection's
	// Receive call.
	ReadTimeout time.Duration
	// WriteTimeout is a timeout on write operations
	WriteTimeout time.Duration
	// ConnectTimeout is a timeout on connect operation
	ConnectTimeout time.Duration
}

// subRequest is an internal request to subscribe or unsubscribe from one or more channels
type subRequest struct {
	channels  []channelID
	subscribe bool
	err       chan error
}

// newSubRequest creates a new request to subscribe or unsubscribe form a channel.
// If the caller cares about response they should set wantResponse and then call
// result() on the request once it has been pushed to the appropriate chan.
func newSubRequest(chIDs []channelID, subscribe bool, wantResponse bool) subRequest {
	r := subRequest{
		channels:  chIDs,
		subscribe: subscribe,
	}
	if wantResponse {
		r.err = make(chan error, 1)
	}
	return r
}

func (sr *subRequest) done(err error) {
	if sr.err == nil {
		return
	}
	sr.err <- err
}

func (sr *subRequest) result() error {
	if sr.err == nil {
		// No waiting, as caller didn't care about response
		return nil
	}
	return <-sr.err
}

func newPool(conf *ShardConfig) *redis.Pool {

	host := conf.Host
	port := conf.Port
	password := conf.Password
	db := conf.DB

	serverAddr := net.JoinHostPort(host, port)
	useSentinel := conf.MasterName != "" && len(conf.SentinelAddrs) > 0

	usingPassword := yesno(password != "")
	if !useSentinel {
		logger.INFO.Printf("Redis: %s/%d, pool: %d, using password: %s\n", serverAddr, db, conf.PoolSize, usingPassword)
	} else {
		logger.INFO.Printf("Redis: Sentinel for name: %s, db: %d, pool: %d, using password: %s\n", conf.MasterName, db, conf.PoolSize, usingPassword)
	}

	var lastMu sync.Mutex
	var lastMaster string

	maxIdle := 10
	if conf.PoolSize < maxIdle {
		maxIdle = conf.PoolSize
	}

	var sntnl *sentinel.Sentinel
	if useSentinel {
		sntnl = &sentinel.Sentinel{
			Addrs:      conf.SentinelAddrs,
			MasterName: conf.MasterName,
			Dial: func(addr string) (redis.Conn, error) {
				timeout := 300 * time.Millisecond
				c, err := redis.DialTimeout("tcp", addr, timeout, timeout, timeout)
				if err != nil {
					logger.CRITICAL.Println(err)
					return nil, err
				}
				return c, nil
			},
		}

		// Periodically discover new Sentinels.
		go func() {
			if err := sntnl.Discover(); err != nil {
				logger.ERROR.Println(err)
			}
			for {
				select {
				case <-time.After(30 * time.Second):
					if err := sntnl.Discover(); err != nil {
						logger.ERROR.Println(err)
					}
				}
			}
		}()
	}

	return &redis.Pool{
		MaxIdle:     maxIdle,
		MaxActive:   conf.PoolSize,
		Wait:        true,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			var err error
			if useSentinel {
				serverAddr, err = sntnl.MasterAddr()
				if err != nil {
					return nil, err
				}
				lastMu.Lock()
				if serverAddr != lastMaster {
					logger.INFO.Printf("Redis master discovered: %s", serverAddr)
					lastMaster = serverAddr
				}
				lastMu.Unlock()
			}

			c, err := redis.DialTimeout("tcp", serverAddr, conf.ConnectTimeout, conf.ReadTimeout, conf.WriteTimeout)
			if err != nil {
				logger.CRITICAL.Println(err)
				return nil, err
			}

			if password != "" {
				if _, err := c.Do("AUTH", password); err != nil {
					c.Close()
					logger.CRITICAL.Println(err)
					return nil, err
				}
			}

			if db != 0 {
				if _, err := c.Do("SELECT", db); err != nil {
					c.Close()
					logger.CRITICAL.Println(err)
					return nil, err
				}
			}

			return c, err
		},
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			if useSentinel {
				if !sentinel.TestRole(c, "master") {
					return errors.New("Failed master role check")
				}
				return nil
			}
			_, err := c.Do("PING")
			return err
		},
	}
}

// New initializes Redis Engine.
func New(n *node.Node, config *Config) (*RedisEngine, error) {

	var shards []*Shard

	if len(config.Shards) > 1 {
		logger.INFO.Printf("Redis sharding enabled: %d shards", len(config.Shards))
	}

	for _, conf := range config.Shards {
		shard, err := NewShard(n, conf)
		if err != nil {
			return nil, err
		}
		shards = append(shards, shard)
	}

	e := &RedisEngine{
		node:     n,
		shards:   shards,
		sharding: len(shards) > 1,
	}
	return e, nil
}

var (
	// pubScriptSource contains lua script we register in Redis to call when publishing
	// client message. It publishes message into channel and adds message to history
	// list maintaining history size and expiration time. This is an optimization to make
	// 1 round trip to Redis instead of 2.
	// KEYS[1] - history list key
	// KEYS[2] - history touch object key
	// ARGV[1] - channel to publish message to
	// ARGV[2] - message payload
	// ARGV[3] - history size
	// ARGV[4] - history lifetime
	// ARGV[5] - history drop inactive flag - "0" or "1"
	pubScriptSource = `
local n = redis.call("publish", ARGV[1], ARGV[2])
local m = 0
if ARGV[5] == "1" and n == 0 and redis.call("exists", KEYS[2]) == 0 then
  m = redis.call("lpushx", KEYS[1], ARGV[2])
else
  m = redis.call("lpush", KEYS[1], ARGV[2])
end
if m > 0 then
  redis.call("ltrim", KEYS[1], 0, ARGV[3])
  redis.call("expire", KEYS[1], ARGV[4])
end
return n
	`

	// KEYS[1] - presence set key
	// KEYS[2] - presence hash key
	// ARGV[1] - key expire seconds
	// ARGV[2] - expire at for set member
	// ARGV[3] - uid
	// ARGV[4] - info payload
	addPresenceSource = `
redis.call("zadd", KEYS[1], ARGV[2], ARGV[3])
redis.call("hset", KEYS[2], ARGV[3], ARGV[4])
redis.call("expire", KEYS[1], ARGV[1])
redis.call("expire", KEYS[2], ARGV[1])
	`

	// KEYS[1] - presence set key
	// KEYS[2] - presence hash key
	// ARGV[1] - uid
	remPresenceSource = `
redis.call("hdel", KEYS[2], ARGV[1])
redis.call("zrem", KEYS[1], ARGV[1])
	`

	// KEYS[1] - presence set key
	// KEYS[2] - presence hash key
	// ARGV[1] - now string
	presenceSource = `
local expired = redis.call("zrangebyscore", KEYS[1], "0", ARGV[1])
if #expired > 0 then
  for num = 1, #expired do
    redis.call("hdel", KEYS[2], expired[num])
  end
  redis.call("zremrangebyscore", KEYS[1], "0", ARGV[1])
end
return redis.call("hgetall", KEYS[2])
	`

	// KEYS[1] - API list (queue) key
	// ARGV[1] - maximum amount of items to get
	lpopManySource = `
local entries = redis.call("lrange", KEYS[1], "0", ARGV[1])
if #entries > 0 then
  redis.call("ltrim", KEYS[1], #entries, -1)
end
return entries
	`
)

// NewShard initializes new Redis shard.
func NewShard(n *node.Node, conf *ShardConfig) (*Shard, error) {
	shard := &Shard{
		node:              n,
		config:            conf,
		pool:              newPool(conf),
		pubScript:         redis.NewScript(2, pubScriptSource),
		addPresenceScript: redis.NewScript(2, addPresenceSource),
		remPresenceScript: redis.NewScript(2, remPresenceSource),
		presenceScript:    redis.NewScript(2, presenceSource),
		lpopManyScript:    redis.NewScript(1, lpopManySource),
	}
	shard.pubCh = make(chan pubRequest, RedisPublishChannelSize)
	shard.subCh = make(chan subRequest, RedisSubscribeChannelSize)
	shard.dataCh = make(chan dataRequest, RedisDataChannelSize)
	shard.messagePrefix = conf.Prefix + RedisClientChannelPrefix
	return shard, nil
}

func yesno(condition bool) string {
	if condition {
		return "yes"
	}
	return "no"
}

func (e *Shard) messageChannelID(ch string) channelID {
	return channelID(e.messagePrefix + ch)
}

func (e *Shard) controlChannelID() channelID {
	return channelID(e.config.Prefix + RedisControlChannelSuffix)
}

func (e *Shard) pingChannelID() channelID {
	return channelID(e.config.Prefix + RedisPingChannelSuffix)
}

func (e *Shard) getPresenceHashKey(ch string) channelID {
	return channelID(e.config.Prefix + ".presence.data." + ch)
}

func (e *Shard) getPresenceSetKey(ch string) channelID {
	return channelID(e.config.Prefix + ".presence.expire." + ch)
}

func (e *Shard) getHistoryKey(ch string) channelID {
	return channelID(e.config.Prefix + ".history.list." + ch)
}

func (e *Shard) getHistoryTouchKey(ch string) channelID {
	return channelID(e.config.Prefix + ".history.touch." + ch)
}

func (e *RedisEngine) shardIndex(channel string) int {
	if !e.sharding {
		return 0
	}
	return consistentIndex(channel, len(e.shards))
}

// Name returns name of engine.
func (e *RedisEngine) Name() string {
	return "Redis"
}

// Run runs engine after node initialized.
func (e *RedisEngine) Run() error {
	for _, shard := range e.shards {
		err := shard.Run()
		if err != nil {
			return err
		}
	}
	return nil
}

// Publish - see engine interface description.
func (e *RedisEngine) Publish(ch string, pub *proto.Publication, opts *channel.Options) <-chan error {
	return e.shards[e.shardIndex(ch)].Publish(ch, pub, opts)
}

// PublishJoin - see engine interface description.
func (e *RedisEngine) PublishJoin(ch string, join *proto.Join, opts *channel.Options) <-chan error {
	return e.shards[e.shardIndex(ch)].PublishJoin(ch, join, opts)
}

// PublishLeave - see engine interface description.
func (e *RedisEngine) PublishLeave(ch string, leave *proto.Leave, opts *channel.Options) <-chan error {
	return e.shards[e.shardIndex(ch)].PublishLeave(ch, leave, opts)
}

// PublishControl - see engine interface description.
func (e *RedisEngine) PublishControl(message *controlproto.Command) <-chan error {
	return e.shards[0].PublishControl(message)
}

// Subscribe - see engine interface description.
func (e *RedisEngine) Subscribe(ch string) error {
	return e.shards[e.shardIndex(ch)].Subscribe(ch)
}

// Unsubscribe - see engine interface description.
func (e *RedisEngine) Unsubscribe(ch string) error {
	return e.shards[e.shardIndex(ch)].Unsubscribe(ch)
}

// AddPresence - see engine interface description.
func (e *RedisEngine) AddPresence(ch string, uid string, info *proto.ClientInfo, expire int) error {
	return e.shards[e.shardIndex(ch)].AddPresence(ch, uid, info, expire)
}

// RemovePresence - see engine interface description.
func (e *RedisEngine) RemovePresence(ch string, uid string) error {
	return e.shards[e.shardIndex(ch)].RemovePresence(ch, uid)
}

// Presence - see engine interface description.
func (e *RedisEngine) Presence(ch string) (map[string]*proto.ClientInfo, error) {
	return e.shards[e.shardIndex(ch)].Presence(ch)
}

// History - see engine interface description.
func (e *RedisEngine) History(ch string, filter engine.HistoryFilter) ([]*proto.Publication, error) {
	return e.shards[e.shardIndex(ch)].History(ch, filter)
}

// RemoveHistory - see engine interface description.
func (e *RedisEngine) RemoveHistory(ch string) error {
	return e.shards[e.shardIndex(ch)].RemoveHistory(ch)
}

// Channels - see engine interface description.
func (e *RedisEngine) Channels() ([]string, error) {
	channelMap := map[string]struct{}{}
	for _, shard := range e.shards {
		chans, err := shard.Channels()
		if err != nil {
			return chans, err
		}
		if !e.sharding {
			// We have all channels on one shard.
			return chans, nil
		}
		for _, ch := range chans {
			channelMap[ch] = struct{}{}
		}
	}
	channels := make([]string, len(channelMap))
	j := 0
	for ch := range channelMap {
		channels[j] = ch
		j++
	}
	return channels, nil
}

// Run runs Redis shard.
func (e *Shard) Run() error {
	go e.runForever(func() {
		e.runPublishPipeline()
	})
	go e.runForever(func() {
		e.runPubSub()
	})
	go e.runForever(func() {
		e.runDataPipeline()
	})
	return nil
}

// Shutdown shuts down Redis engine.
func (e *RedisEngine) Shutdown() error {
	return errors.New("Shutdown not implemented")
}

// runForever simple keeps another function running indefinitely
// the reason this loop is not inside the function itself is so that defer
// can be used to cleanup nicely (defers only run at function return not end of block scope)
func (e *Shard) runForever(fn func()) {
	shutdownCh := e.node.NotifyShutdown()
	for {
		select {
		case <-shutdownCh:
			return
		default:
			fn()
		}
		// Sleep for a while to prevent busy loop when reconnecting to Redis.
		time.Sleep(300 * time.Millisecond)
	}
}

func (e *Shard) blpopTimeout() int {
	var timeout int
	e.RLock()
	readTimeout := e.config.ReadTimeout
	e.RUnlock()
	if readTimeout == 0 {
		// No read timeout - we can block forever in BLPOP.
		timeout = 0
	} else {
		timeout = int(readTimeout.Seconds() / 2)
		if timeout == 0 {
			timeout = 1
		}
	}
	return timeout
}

func (e *Shard) runPubSub() {

	e.RLock()
	numWorkers := e.config.PubSubNumWorkers
	e.RUnlock()
	if numWorkers == 0 {
		numWorkers = runtime.NumCPU()
	}

	logger.DEBUG.Printf("Running Redis PUB/SUB, num workers: %d", numWorkers)
	defer func() {
		logger.DEBUG.Printf("Stopping Redis PUB/SUB")
	}()

	poolConn := e.pool.Get()
	if poolConn.Err() != nil {
		// At this moment test on borrow could already return an error,
		// we can't work with broken connection.
		poolConn.Close()
		return
	}

	conn := redis.PubSubConn{Conn: poolConn}
	defer conn.Close()

	done := make(chan struct{})
	defer close(done)

	// Run subscriber goroutine.
	go func() {
		logger.DEBUG.Println("Starting RedisEngine Subscriber")

		defer func() {
			logger.DEBUG.Println("Stopping RedisEngine Subscriber")
		}()
		for {
			select {
			case <-done:
				return
			case r := <-e.subCh:
				chIDs := make([]interface{}, len(r.channels))
				i := 0
				for _, ch := range r.channels {
					chIDs[i] = ch
					i++
				}

				var opErr error
				if r.subscribe {
					opErr = conn.Subscribe(chIDs...)
				} else {
					opErr = conn.Unsubscribe(chIDs...)
				}

				if opErr != nil {
					logger.ERROR.Printf("RedisEngine Subscriber error: %v\n", opErr)
					r.done(opErr)

					// Close conn, this should cause Receive to return with err below
					// and whole runPubSub method to restart.
					conn.Close()
					return
				}
				r.done(nil)
			}
		}
	}()

	controlChannel := e.controlChannelID()
	pingChannel := e.pingChannelID()

	// Run workers to spread received message processing work over worker goroutines.
	workers := make(map[int]chan redis.Message)
	for i := 0; i < numWorkers; i++ {
		workerCh := make(chan redis.Message, RedisPubSubWorkerChannelSize)
		workers[i] = workerCh
		go func(ch chan redis.Message) {
			for {
				select {
				case <-done:
					return
				case n := <-ch:
					chID := channelID(n.Channel)
					if len(n.Data) == 0 {
						continue
					}
					switch chID {
					case controlChannel:
						cmd, err := e.node.ControlDecoder().DecodeCommand(n.Data)
						if err != nil {
							logger.ERROR.Println(err)
							continue
						}
						e.node.HandleControl(cmd)
					case pingChannel:
						// Do nothing - this message just maintains connection open.
					default:
						err := e.handleRedisClientMessage(chID, n.Data)
						if err != nil {
							logger.ERROR.Println(err)
							continue
						}
					}
				}
			}
		}(workerCh)
	}

	chIDs := make([]channelID, 2)
	chIDs[0] = controlChannel
	chIDs[1] = pingChannel

	for _, ch := range e.node.Hub().Channels() {
		chIDs = append(chIDs, e.messageChannelID(ch))
	}

	batch := make([]channelID, 0)

	for i, ch := range chIDs {
		if len(batch) > 0 && i%RedisSubscribeBatchLimit == 0 {
			r := newSubRequest(batch, true, true)
			e.subCh <- r
			err := r.result()
			if err != nil {
				logger.ERROR.Printf("Error subscribing: %v", err)
				return
			}
			batch = nil
		}
		batch = append(batch, ch)
	}
	if len(batch) > 0 {
		r := newSubRequest(batch, true, true)
		e.subCh <- r
		err := r.result()
		if err != nil {
			logger.ERROR.Printf("Error subscribing: %v", err)
			return
		}
	}

	logger.DEBUG.Printf("Successfully subscribed to %d Redis channels", len(chIDs))

	for {
		switch n := conn.Receive().(type) {
		case redis.Message:
			// Add message to worker channel preserving message order - i.e. messages from
			// the same channel will be processed in the same worker.
			workers[index(n.Channel, numWorkers)] <- n
		case redis.Subscription:
		case error:
			logger.ERROR.Printf("Redis receiver error: %v\n", n)
			return
		}
	}
}

func (e *Shard) handleRedisClientMessage(chID channelID, data []byte) error {
	var message proto.Message
	err := message.Unmarshal(data)
	if err != nil {
		return err
	}
	return e.node.HandleClientMessage(&message)
}

type pubRequest struct {
	channel    channelID
	message    []byte
	historyKey channelID
	touchKey   channelID
	opts       *channel.Options
	err        *chan error
}

func (pr *pubRequest) done(err error) {
	*(pr.err) <- err
}

func (pr *pubRequest) result() error {
	return <-*(pr.err)
}

func fillPublishBatch(ch chan pubRequest, prs *[]pubRequest) {
	for len(*prs) < RedisPublishBatchLimit {
		select {
		case pr := <-ch:
			*prs = append(*prs, pr)
		default:
			return
		}
	}
}

func (e *Shard) runPublishPipeline() {
	conn := e.pool.Get()

	err := e.pubScript.Load(conn)
	if err != nil {
		logger.ERROR.Println(err)
		// Can not proceed if script has not been loaded - because we use EVALSHA command for
		// publishing with history.
		conn.Close()
		return
	}

	conn.Close()

	var prs []pubRequest

	e.RLock()
	pingTimeout := e.config.ReadTimeout / 3
	e.RUnlock()

	for {
		select {
		case <-time.After(pingTimeout):
			// We have to PUBLISH pings into connection to prevent connection close after read timeout.
			// In our case it's important to maintain PUB/SUB receiver connection alive to prevent
			// resubscribing on all our subscriptions again and again.
			conn := e.pool.Get()
			err := conn.Send("PUBLISH", e.pingChannelID(), nil)
			if err != nil {
				logger.ERROR.Printf("Error publish ping: %v", err)
				conn.Close()
				return
			}
			conn.Close()
		case pr := <-e.pubCh:
			prs = append(prs, pr)
			fillPublishBatch(e.pubCh, &prs)
			conn := e.pool.Get()
			for i := range prs {
				if prs[i].opts != nil && prs[i].opts.HistorySize > 0 && prs[i].opts.HistoryLifetime > 0 {
					e.pubScript.SendHash(conn, prs[i].historyKey, prs[i].touchKey, prs[i].channel, prs[i].message, prs[i].opts.HistorySize, prs[i].opts.HistoryLifetime, prs[i].opts.HistoryDropInactive)
				} else {
					conn.Send("PUBLISH", prs[i].channel, prs[i].message)
				}
			}
			err := conn.Flush()
			if err != nil {
				for i := range prs {
					prs[i].done(err)
				}
				logger.ERROR.Printf("Error flushing publish pipeline: %v", err)
				conn.Close()
				return
			}
			var noScriptError bool
			for i := range prs {
				_, err := conn.Receive()
				if err != nil {
					// Check for NOSCRIPT error. In normal circumstances this should never happen.
					// The only possible situation is when Redis scripts were flushed. In this case
					// we will return from this func and load publish script from scratch.
					// Redigo does the same check but for single EVALSHA command: see
					// https://github.com/garyburd/redigo/blob/master/redis/script.go#L64
					if e, ok := err.(redis.Error); ok && strings.HasPrefix(string(e), "NOSCRIPT ") {
						noScriptError = true
					}
				}
				prs[i].done(err)
			}
			if noScriptError {
				// Start this func from the beginning and LOAD missing script.
				conn.Close()
				return
			}
			conn.Close()
			prs = nil
		}
	}
}

type dataOp int

const (
	dataOpAddPresence dataOp = iota
	dataOpRemovePresence
	dataOpPresence
	dataOpHistory
	dataOpChannels
	dataOpHistoryTouch
)

type dataResponse struct {
	reply interface{}
	err   error
}

type dataRequest struct {
	op   dataOp
	args []interface{}
	resp chan *dataResponse
}

func newDataRequest(op dataOp, args []interface{}, wantResponse bool) dataRequest {
	r := dataRequest{op: op, args: args}
	if wantResponse {
		r.resp = make(chan *dataResponse, 1)
	}
	return r
}

func (dr *dataRequest) done(reply interface{}, err error) {
	if dr.resp == nil {
		return
	}
	dr.resp <- &dataResponse{reply: reply, err: err}
}

func (dr *dataRequest) result() *dataResponse {
	if dr.resp == nil {
		// No waiting, as caller didn't care about response.
		return &dataResponse{}
	}
	return <-dr.resp
}

func fillDataBatch(ch <-chan dataRequest, batch *[]dataRequest, maxSize int) {
	for len(*batch) < maxSize {
		select {
		case req := <-ch:
			*batch = append(*batch, req)
		default:
			return
		}
	}
}

func (e *Shard) runDataPipeline() {

	conn := e.pool.Get()

	err := e.addPresenceScript.Load(conn)
	if err != nil {
		logger.ERROR.Println(err)
		// Can not proceed if script has not been loaded.
		conn.Close()
		return
	}

	err = e.presenceScript.Load(conn)
	if err != nil {
		logger.ERROR.Println(err)
		// Can not proceed if script has not been loaded.
		conn.Close()
		return
	}

	err = e.remPresenceScript.Load(conn)
	if err != nil {
		logger.ERROR.Println(err)
		// Can not proceed if script has not been loaded.
		conn.Close()
		return
	}

	conn.Close()

	var drs []dataRequest

	for {
		select {
		case dr := <-e.dataCh:
			drs = append(drs, dr)
			fillDataBatch(e.dataCh, &drs, RedisDataChannelSize)

			conn := e.pool.Get()

			for i := range drs {
				switch drs[i].op {
				case dataOpAddPresence:
					e.addPresenceScript.SendHash(conn, drs[i].args...)
				case dataOpRemovePresence:
					e.remPresenceScript.SendHash(conn, drs[i].args...)
				case dataOpPresence:
					e.presenceScript.SendHash(conn, drs[i].args...)
				case dataOpHistory:
					conn.Send("LRANGE", drs[i].args...)
				case dataOpChannels:
					conn.Send("PUBSUB", drs[i].args...)
				case dataOpHistoryTouch:
					conn.Send("SETEX", drs[i].args...)
				}
			}

			err := conn.Flush()
			if err != nil {
				for i := range drs {
					drs[i].done(nil, err)
				}
				logger.ERROR.Printf("Error flushing publish pipeline: %v", err)
				conn.Close()
				return
			}
			var noScriptError bool
			for i := range drs {
				reply, err := conn.Receive()
				if err != nil {
					// Check for NOSCRIPT error. In normal circumstances this should never happen.
					// The only possible situation is when Redis scripts were flushed. In this case
					// we will return from this func and load publish script from scratch.
					// Redigo does the same check but for single EVALSHA command: see
					// https://github.com/garyburd/redigo/blob/master/redis/script.go#L64
					if e, ok := err.(redis.Error); ok && strings.HasPrefix(string(e), "NOSCRIPT ") {
						noScriptError = true
					}
				}
				drs[i].done(reply, err)
			}
			if noScriptError {
				// Start this func from the beginning and LOAD missing script.
				conn.Close()
				return
			}
			conn.Close()
			drs = nil
		}
	}
}

// Publish - see engine interface description.
func (e *Shard) Publish(ch string, pub *proto.Publication, opts *channel.Options) <-chan error {

	eChan := make(chan error, 1)

	data, err := e.node.MessageEncoder().EncodePublication(pub)
	if err != nil {
		eChan <- err
		return eChan
	}
	byteMessage, err := e.node.MessageEncoder().Encode(proto.NewPublicationMessage(ch, data))
	if err != nil {
		eChan <- err
		return eChan
	}

	chID := e.messageChannelID(ch)

	if opts != nil && opts.HistorySize > 0 && opts.HistoryLifetime > 0 {
		pr := pubRequest{
			channel:    chID,
			message:    byteMessage,
			historyKey: e.getHistoryKey(ch),
			touchKey:   e.getHistoryTouchKey(ch),
			opts:       opts,
			err:        &eChan,
		}
		e.pubCh <- pr
		return eChan
	}

	pr := pubRequest{
		channel: chID,
		message: byteMessage,
		err:     &eChan,
	}
	e.pubCh <- pr
	return eChan
}

// PublishJoin - see engine interface description.
func (e *Shard) PublishJoin(ch string, join *proto.Join, opts *channel.Options) <-chan error {

	eChan := make(chan error, 1)

	data, err := e.node.MessageEncoder().EncodeJoin(join)
	if err != nil {
		eChan <- err
		return eChan
	}
	byteMessage, err := e.node.MessageEncoder().Encode(proto.NewJoinMessage(ch, data))
	if err != nil {
		eChan <- err
		return eChan
	}

	chID := e.messageChannelID(ch)

	pr := pubRequest{
		channel: chID,
		message: byteMessage,
		err:     &eChan,
	}
	e.pubCh <- pr
	return eChan
}

// PublishLeave - see engine interface description.
func (e *Shard) PublishLeave(ch string, leave *proto.Leave, opts *channel.Options) <-chan error {

	eChan := make(chan error, 1)

	data, err := e.node.MessageEncoder().EncodeLeave(leave)
	if err != nil {
		eChan <- err
		return eChan
	}
	byteMessage, err := e.node.MessageEncoder().Encode(proto.NewLeaveMessage(ch, data))
	if err != nil {
		eChan <- err
		return eChan
	}

	chID := e.messageChannelID(ch)

	pr := pubRequest{
		channel: chID,
		message: byteMessage,
		err:     &eChan,
	}
	e.pubCh <- pr
	return eChan
}

// PublishControl - see engine interface description.
func (e *Shard) PublishControl(cmd *controlproto.Command) <-chan error {
	eChan := make(chan error, 1)

	byteMessage, err := e.node.ControlEncoder().EncodeCommand(cmd)
	if err != nil {
		eChan <- err
		return eChan
	}

	chID := e.controlChannelID()

	pr := pubRequest{
		channel: chID,
		message: byteMessage,
		err:     &eChan,
	}
	e.pubCh <- pr
	return eChan
}

// Subscribe - see engine interface description.
func (e *Shard) Subscribe(ch string) error {
	logger.DEBUG.Println("Subscribe node on channel", ch)
	channel := e.messageChannelID(ch)
	r := newSubRequest([]channelID{channel}, true, true)
	e.subCh <- r
	return r.result()
}

// Unsubscribe - see engine interface description.
func (e *Shard) Unsubscribe(ch string) error {
	logger.DEBUG.Println("Unsubscribe node from channel", ch)
	channel := e.messageChannelID(ch)
	r := newSubRequest([]channelID{channel}, false, true)
	e.subCh <- r

	if chOpts, ok := e.node.ChannelOpts(ch); ok && chOpts.HistoryDropInactive {
		// Waiting for response here is not actually required. But this seems
		// semantically correct and allows avoid races in drop inactive tests.
		// It does not seem a big bottleneck for real usage but can be tuned in
		// future if we find any problems with it.
		dr := newDataRequest(dataOpHistoryTouch, []interface{}{e.getHistoryTouchKey(ch), chOpts.HistoryLifetime, ""}, true)
		e.dataCh <- dr
		dr.result()
	}
	return r.result()
}

// AddPresence - see engine interface description.
func (e *Shard) AddPresence(ch string, uid string, info *proto.ClientInfo, expire int) error {
	infoJSON, err := info.Marshal()
	if err != nil {
		return err
	}
	expireAt := time.Now().Unix() + int64(expire)
	hashKey := e.getPresenceHashKey(ch)
	setKey := e.getPresenceSetKey(ch)
	dr := newDataRequest(dataOpAddPresence, []interface{}{setKey, hashKey, expire, expireAt, uid, infoJSON}, true)
	e.dataCh <- dr
	resp := dr.result()
	return resp.err
}

// RemovePresence - see engine interface description.
func (e *Shard) RemovePresence(ch string, uid string) error {
	hashKey := e.getPresenceHashKey(ch)
	setKey := e.getPresenceSetKey(ch)
	dr := newDataRequest(dataOpRemovePresence, []interface{}{setKey, hashKey, uid}, true)
	e.dataCh <- dr
	resp := dr.result()
	return resp.err
}

// Presence - see engine interface description.
func (e *Shard) Presence(ch string) (map[string]*proto.ClientInfo, error) {
	hashKey := e.getPresenceHashKey(ch)
	setKey := e.getPresenceSetKey(ch)
	now := int(time.Now().Unix())
	dr := newDataRequest(dataOpPresence, []interface{}{setKey, hashKey, now}, true)
	e.dataCh <- dr
	resp := dr.result()
	if resp.err != nil {
		return nil, resp.err
	}
	return mapStringClientInfo(resp.reply, nil)
}

// History - see engine interface description.
func (e *Shard) History(ch string, filter engine.HistoryFilter) ([]*proto.Publication, error) {
	limit := filter.Limit
	var rangeBound = -1
	if limit > 0 {
		rangeBound = limit - 1 // Redis includes last index into result
	}
	historyKey := e.getHistoryKey(ch)
	dr := newDataRequest(dataOpHistory, []interface{}{historyKey, 0, rangeBound}, true)
	e.dataCh <- dr
	resp := dr.result()
	if resp.err != nil {
		return nil, resp.err
	}
	return sliceOfMessages(e.node, resp.reply, nil)
}

// RemoveHistory - see engine interface description.
// TODO
func (e *Shard) RemoveHistory(ch string) error {
	return nil
}

// Channels - see engine interface description.
// Requires Redis >= 2.8.0 (http://redis.io/commands/pubsub)
func (e *Shard) Channels() ([]string, error) {
	dr := newDataRequest(dataOpChannels, []interface{}{"CHANNELS", e.messagePrefix + "*"}, true)
	e.dataCh <- dr
	resp := dr.result()
	if resp.err != nil {
		return nil, resp.err
	}
	values, err := redis.Values(resp.reply, nil)
	if err != nil {
		return nil, err
	}
	channels := make([]string, 0, len(values))
	for i := 0; i < len(values); i++ {
		value, okValue := values[i].([]byte)
		if !okValue {
			return nil, errors.New("error getting channelID value")
		}
		chID := channelID(value)
		channels = append(channels, string(string(chID)[len(e.messagePrefix):]))
	}
	return channels, nil
}
