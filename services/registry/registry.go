// Package registry maintains the set of Vouch validator pubkeys (the "Vouch
// registry") used by the protocol fee-recipient enforcement predicate.
//
// The registry is sourced from the on-chain NodeDeposit contract. Enumeration
// deliberately uses the per-index getter (pubkeysOfNode) — never getPubkeysOfNode,
// whose quadratic memory-expansion cost crosses the block-gas wall at ~4,500
// pubkeys per node. The set is cached in-memory and mirrored to Redis for
// last-known-good recovery across restarts and relay instances.
package registry

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/flashbots/go-boost-utils/utils"
	"github.com/flashbots/mev-boost-relay/datastore"
	"github.com/sirupsen/logrus"
)

// nodeDepositABI is the minimal NodeDeposit ABI needed for registry enumeration.
// Functions: getNodesLength(), getNodes(_start,_end), pubkeysOfNode(node,i).
const nodeDepositABI = `[
  {"inputs":[],"name":"getNodesLength","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
  {"inputs":[{"internalType":"uint256","name":"_start","type":"uint256"},{"internalType":"uint256","name":"_end","type":"uint256"}],"name":"getNodes","outputs":[{"internalType":"address[]","name":"nodeList","type":"address[]"}],"stateMutability":"view","type":"function"},
  {"inputs":[{"internalType":"address","name":"","type":"address"},{"internalType":"uint256","name":"","type":"uint256"}],"name":"pubkeysOfNode","outputs":[{"internalType":"bytes","name":"","type":"bytes"}],"stateMutability":"view","type":"function"}
]`

// defaultRefreshInterval is used when no interval is configured.
const defaultRefreshInterval = 5 * time.Minute

// Opts configures a Registry.
type Opts struct {
	Log               *logrus.Entry
	RPCURL            string
	ContractAddress   common.Address
	Redis             *datastore.RedisCache
	RefreshInterval   time.Duration
	StaticPubkeysFile string
}

// Registry holds the in-memory set of Vouch validator pubkeys and refreshes it
// from NodeDeposit on an interval.
type Registry struct {
	log          *logrus.Entry
	rpcClient    *rpc.Client
	contractAddr common.Address
	parsedABI    abi.ABI
	redis        *datastore.RedisCache
	refresh      time.Duration

	mu      sync.RWMutex
	pubkeys map[string]struct{} // lowercase 0x-prefixed hex pubkeys

	syncing atomic.Bool
	stop    chan struct{}
}

// New creates a Registry. If RPCURL is empty, the registry can only be populated
// from the static pubkeys file or Redis (used by tests and by the fail-open
// default). An invalid static file path or RPC dial error is fatal.
func New(opts Opts) (*Registry, error) {
	r := &Registry{
		log:     opts.Log,
		redis:   opts.Redis,
		refresh: opts.RefreshInterval,
		pubkeys: make(map[string]struct{}),
		stop:    make(chan struct{}),
	}
	if r.log == nil {
		r.log = logrus.NewEntry(logrus.New())
	}
	if r.refresh <= 0 {
		r.refresh = defaultRefreshInterval
	}

	if opts.StaticPubkeysFile != "" {
		pubkeys, err := loadPubkeysFile(opts.StaticPubkeysFile)
		if err != nil {
			return nil, err
		}
		r.SetPubkeys(pubkeys)
	}

	if opts.RPCURL != "" {
		rpcClient, err := rpc.DialContext(context.Background(), opts.RPCURL)
		if err != nil {
			return nil, err
		}
		parsedABI, err := abi.JSON(strings.NewReader(nodeDepositABI))
		if err != nil {
			return nil, err
		}
		r.rpcClient = rpcClient
		r.contractAddr = opts.ContractAddress
		r.parsedABI = parsedABI
	}

	// Last-known-good from Redis (survives restarts; never cleared on failure).
	if r.redis != nil {
		if saved, err := r.redis.GetVouchRegistry(); err == nil && len(saved) > 0 {
			r.SetPubkeys(saved)
		} else if err != nil {
			r.log.WithError(err).Warn("could not load vouch registry from redis")
		}
	}

	return r, nil
}

// Start launches the background refresh loop (blocks are not waited on).
func (r *Registry) Start() {
	go r.loop()
}

// Stop halts the background refresh loop.
func (r *Registry) Stop() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
}

func (r *Registry) loop() {
	r.sync()
	ticker := time.NewTicker(r.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.sync()
		case <-r.stop:
			return
		}
	}
}

// sync re-enumerates the registry and, on success, replaces the in-memory set
// and the Redis copy. On RPC failure the previous set is kept (last-known-good).
func (r *Registry) sync() {
	if r.rpcClient == nil {
		return
	}
	if !r.syncing.CompareAndSwap(false, true) {
		return // a sync is already in progress
	}
	defer r.syncing.Store(false)

	// Bound the enumeration so a hung RPC cannot block the next refresh behind
	// the sync-in-progress guard; the refresh cadence is the deadline.
	ctx, cancel := context.WithTimeout(context.Background(), r.refresh)
	defer cancel()

	pubkeys, err := r.enumerateContext(ctx)
	if err != nil {
		r.log.WithError(err).Error("vouch registry sync failed; keeping last-known-good set")
		return
	}
	r.SetPubkeys(pubkeys)
	if r.redis != nil {
		if err := r.redis.SetVouchRegistry(pubkeys); err != nil {
			r.log.WithError(err).Error("failed to persist vouch registry to redis")
		}
	}
	r.log.WithField("pubkeys", len(pubkeys)).Info("vouch registry synced")
}

// enumerate reads the Vouch pubkey set from the bound contract via the per-index
// getter (never getPubkeysOfNode — quadratic memory wall).
func (r *Registry) enumerate() ([]string, error) {
	return r.enumerateContext(context.Background())
}

func (r *Registry) enumerateContext(ctx context.Context) ([]string, error) {
	if r.rpcClient == nil {
		return nil, errors.New("no RPC configured")
	}
	pubkeys, _, err := enumerateWithClient(ctx, r.rpcClient, r.contractAddr, r.parsedABI)
	return pubkeys, err
}

// EnumeratePubkeys dials the EL RPC and enumerates the Vouch pubkey set from
// NodeDeposit via the per-index getter (never getPubkeysOfNode). Returns the
// pubkeys normalized to lowercase 0x-prefixed hex and the number of nodes
// enumerated. Any RPC failure (other than the end-of-list revert) returns an
// error — callers (e.g. the export tool) must treat the result as incomplete.
func EnumeratePubkeys(ctx context.Context, rpcURL string, contractAddr common.Address) (pubkeys []string, nodes int, err error) {
	rpcClient, err := rpc.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, 0, fmt.Errorf("could not dial RPC: %w", err)
	}
	parsedABI, err := abi.JSON(strings.NewReader(nodeDepositABI))
	if err != nil {
		return nil, 0, err
	}
	return enumerateWithClient(ctx, rpcClient, contractAddr, parsedABI)
}

// enumerationWorkers bounds the number of concurrent node walks (max in-flight
// HTTP requests). 8 matches the workspace's established parallel-batch pattern.
const enumerationWorkers = 8

// pubkeysBatchSize is the number of per-index getter calls packed into one
// JSON-RPC batch request per node walk, amortizing network round-trips.
const pubkeysBatchSize = 64

// enumerateWithClient reads the Vouch pubkey set from NodeDeposit via the
// per-index getter across all nodes. Node walks run in parallel across a bounded
// worker pool; each node's indices are walked sequentially in JSON-RPC batches
// until the end-of-list revert (there is no per-node length getter on the
// contract). The context cancels all in-flight calls and the walk.
func enumerateWithClient(ctx context.Context, rpcClient *rpc.Client, contractAddr common.Address, parsedABI abi.ABI) ([]string, int, error) {
	res, err := callView(ctx, rpcClient, contractAddr, parsedABI, "getNodesLength")
	if err != nil {
		return nil, 0, fmt.Errorf("getNodesLength: %w", err)
	}
	if len(res) == 0 {
		return []string{}, 0, nil
	}
	nodesLen, _ := res[0].(*big.Int)
	if nodesLen == nil || nodesLen.Sign() == 0 {
		return []string{}, 0, nil
	}

	res, err = callView(ctx, rpcClient, contractAddr, parsedABI, "getNodes", big.NewInt(0), nodesLen)
	if err != nil {
		return nil, 0, fmt.Errorf("getNodes: %w", err)
	}
	if len(res) == 0 {
		return []string{}, 0, nil
	}
	nodes, _ := res[0].([]common.Address)
	nodeCount := len(nodes)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	set := make(map[string]struct{})
	var mu sync.Mutex
	jobs := make(chan common.Address)
	var wg sync.WaitGroup
	workers := enumerationWorkers
	if nodeCount < workers {
		workers = nodeCount
	}
	if workers < 1 {
		workers = 1
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for node := range jobs {
				if err := walkNodePubkeys(ctx, rpcClient, contractAddr, parsedABI, node, set, &mu); err != nil {
					if ctx.Err() != nil {
						return // cancelled; the producer stops sending
					}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, node := range nodes {
			select {
			case <-ctx.Done():
				return
			case jobs <- node:
			}
		}
	}()
	wg.Wait()
	if ctx.Err() != nil {
		return nil, nodeCount, ctx.Err()
	}

	pubkeys := make([]string, 0, len(set))
	for pk := range set {
		pubkeys = append(pubkeys, pk)
	}
	sort.Strings(pubkeys)
	return pubkeys, nodeCount, nil
}

// callView packs and executes a single view-function eth_call, returning the
// unpacked outputs.
func callView(ctx context.Context, rpcClient *rpc.Client, contractAddr common.Address, parsedABI abi.ABI, method string, args ...interface{}) ([]interface{}, error) {
	data, err := parsedABI.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	var out hexutil.Bytes
	if err := rpcClient.CallContext(ctx, &out, "eth_call", map[string]interface{}{"to": contractAddr, "data": hexutil.Bytes(data)}, "latest"); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return parsedABI.Unpack(method, out)
}

// walkNodePubkeys walks one node's pubkey list sequentially via the per-index
// getter, in JSON-RPC batches. The end-of-list revert (or an empty result) ends
// the walk; any other error is a real RPC failure and is propagated.
func walkNodePubkeys(ctx context.Context, rpcClient *rpc.Client, contractAddr common.Address, parsedABI abi.ABI, node common.Address, set map[string]struct{}, mu *sync.Mutex) error {
	for start := uint64(0); ; start += pubkeysBatchSize {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		elems := make([]rpc.BatchElem, pubkeysBatchSize)
		for j := range elems {
			idx := start + uint64(j)
			data, err := parsedABI.Pack("pubkeysOfNode", node, new(big.Int).SetUint64(idx))
			if err != nil {
				return err
			}
			var out hexutil.Bytes
			elems[j] = rpc.BatchElem{
				Method: "eth_call",
				Args:   []interface{}{map[string]interface{}{"to": contractAddr, "data": hexutil.Bytes(data)}, "latest"},
				Result: &out,
			}
		}
		if err := rpcClient.BatchCallContext(ctx, elems); err != nil {
			return fmt.Errorf("pubkeysOfNode batch (%s, %d..%d): %w", node, start, start+pubkeysBatchSize-1, err)
		}

		for j := range elems {
			if elems[j].Error != nil {
				if isRevertError(elems[j].Error) {
					return nil // out-of-range index reverts → end of this node's list
				}
				return fmt.Errorf("pubkeysOfNode(%s, %d): %w", node, start+uint64(j), elems[j].Error)
			}
			out, ok := elems[j].Result.(*hexutil.Bytes)
			if !ok || out == nil {
				return fmt.Errorf("pubkeysOfNode(%s, %d): unexpected batch result type", node, start+uint64(j))
			}
			raw := *out
			if len(raw) == 0 {
				return nil
			}
			unpacked, err := parsedABI.Unpack("pubkeysOfNode", raw)
			if err != nil {
				return fmt.Errorf("pubkeysOfNode(%s, %d) decode: %w", node, start+uint64(j), err)
			}
			if len(unpacked) == 0 {
				return nil
			}
			pk, _ := unpacked[0].([]byte)
			if len(pk) == 0 {
				return nil
			}
			mu.Lock()
			set["0x"+fmt.Sprintf("%x", pk)] = struct{}{}
			mu.Unlock()
		}
	}
}

// isRevertError reports whether the error is a contract revert (the end-of-list
// signal for the per-index getter) rather than a transport/RPC failure.
func isRevertError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "revert")
}

// IsVouchPubkey reports whether the given pubkey is in the Vouch registry.
// The check is case-insensitive (keys are normalized to lowercase 0x hex).
func (r *Registry) IsVouchPubkey(pubkey string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.pubkeys[strings.ToLower(pubkey)]
	return ok
}

// SetPubkeys atomically replaces the in-memory set (used by the static file
// fallback, Redis load, sync results, and tests).
func (r *Registry) SetPubkeys(pubkeys []string) {
	set := make(map[string]struct{}, len(pubkeys))
	for _, pk := range pubkeys {
		set[strings.ToLower(pk)] = struct{}{}
	}
	r.mu.Lock()
	r.pubkeys = set
	r.mu.Unlock()
}

func loadPubkeysFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read vouch pubkeys file %s: %w", path, err)
	}
	var pubkeys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pubkeys = append(pubkeys, line)
	}
	return pubkeys, nil
}

// WritePubkeysFile writes the pubkeys in exactly the format loadPubkeysFile
// reads: one 0x-prefixed hex pubkey per line (lowercase, sorted), blank lines and
// `#` comment lines tolerated on read. Duplicates are dropped.
func WritePubkeysFile(path string, pubkeys []string) error {
	set := make(map[string]struct{}, len(pubkeys))
	for _, pk := range pubkeys {
		set[strings.ToLower(pk)] = struct{}{}
	}
	unique := make([]string, 0, len(set))
	for pk := range set {
		unique = append(unique, pk)
	}
	sort.Strings(unique)

	var b strings.Builder
	for _, pk := range unique {
		b.WriteString(pk)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// ParseAddress validates a hex address and returns it as an execution address.
// Invalid hex returns an error (callers are expected to fail startup on it).
func ParseAddress(hex string) (bellatrix.ExecutionAddress, error) {
	return utils.HexToAddress(hex)
}
