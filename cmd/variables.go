package cmd

import (
	"os"

	"github.com/flashbots/mev-boost-relay/common"
)

var (
	defaultNetwork           = common.GetEnv("NETWORK", "")
	defaultBeaconURIs        = common.GetSliceEnv("BEACON_URIS", []string{"http://localhost:3500"})
	defaultBeaconPublishURIs = common.GetSliceEnv("BEACON_PUBLISH_URIS", []string{})
	defaultRedisURI          = common.GetEnv("REDIS_URI", "localhost:6379")
	defaultRedisReadonlyURI  = common.GetEnv("REDIS_READONLY_URI", "")
	defaultPostgresDSN       = common.GetEnv("POSTGRES_DSN", "")
	defaultMemcachedURIs     = common.GetSliceEnv("MEMCACHED_URIS", nil)
	defaultLogJSON           = os.Getenv("LOG_JSON") != ""
	defaultLogLevel          = common.GetEnv("LOG_LEVEL", "info")

	defaultProtocolFeeRecipient    = common.GetEnv("PROTOCOL_FEE_RECIPIENT", "")
	defaultVouchRegistryAddress    = common.GetEnv("VOUCH_REGISTRY_ADDRESS", "")
	defaultRegistryRefreshInterval = common.GetEnv("REGISTRY_REFRESH_INTERVAL", "5m")
	defaultVouchPubkeysFile        = common.GetEnv("VOUCH_PUBKEYS_FILE", "")
	defaultVouchRegistryRPC        = common.GetEnv("VOUCH_REGISTRY_RPC", "")

	beaconNodeURIs        []string
	beaconNodePublishURIs []string
	redisURI              string
	redisReadonlyURI      string
	postgresDSN           string
	memcachedURIs         []string

	logJSON  bool
	logLevel string

	network string

	protocolFeeRecipient    string
	vouchRegistryAddress    string
	registryRefreshInterval string
	vouchPubkeysFile        string
	vouchRegistryRPC        string
)
