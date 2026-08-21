package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/flashbots/mev-boost-relay/datastore"
	"github.com/stretchr/testify/require"
)

const (
	testVouchPubkey    = "0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249"
	testExternalPubkey = "0xb872a4f5f596ea7dfd695e45afbe4551b405b10dafba98b2d897c58a5047fc288ef2c1bc4216f906ea05d7fdbed61116"
	testVFDAddress     = "0x9325008eE3B5982c10010C8f12b6CD4943F48fA6"
	testForeignAddress = "0x95222290DD7278Aa3Ddd389Cc1E1d165CC4BAfe5"
)

func TestParseAddress(t *testing.T) {
	// valid mixed-case hex normalizes to the same 20 bytes
	addr, err := ParseAddress(testVFDAddress)
	require.NoError(t, err)
	require.Equal(t, testVFDAddress, addr.String())

	// invalid hex is an error (callers fail startup on it)
	_, err = ParseAddress("0xzzz")
	require.Error(t, err)
	_, err = ParseAddress("not-hex")
	require.Error(t, err)
}

func TestIsVouchPubkeyCaseInsensitive(t *testing.T) {
	reg, err := New(Opts{})
	require.NoError(t, err)

	// stored mixed-case
	reg.SetPubkeys([]string{"0x8A1d7B8dD64e0AaFe7Ea7B6c95065C9364CF99D38470C12EE807D55F7dE1529ad29CE2c422E0B65E3D5a05C02caCa249"})

	// lookup lowercase
	require.True(t, reg.IsVouchPubkey(testVouchPubkey))
	// lookup different case
	require.True(t, reg.IsVouchPubkey("0x8A1D7B8DD64E0AAFE7EA7B6C95065C9364CF99D38470C12EE807D55F7DE1529AD29CE2C422E0B65E3D5A05C02CACA249"))
	// unknown
	require.False(t, reg.IsVouchPubkey(testExternalPubkey))
}

func TestLoadPubkeysFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pubkeys.txt")
	content := "# comment line\n" + testVouchPubkey + "\n\n" + testExternalPubkey + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	reg, err := New(Opts{StaticPubkeysFile: path})
	require.NoError(t, err)
	require.True(t, reg.IsVouchPubkey(testVouchPubkey))
	require.True(t, reg.IsVouchPubkey(testExternalPubkey))
	require.False(t, reg.IsVouchPubkey("0x000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"))

	// invalid file path is fatal at New
	_, err = New(Opts{StaticPubkeysFile: filepath.Join(dir, "missing.txt")})
	require.Error(t, err)
}

func TestRedisLastKnownGood(t *testing.T) {
	s := miniredis.RunT(t)
	redis, err := datastore.NewRedisCache("testnet", s.Addr(), "")
	require.NoError(t, err)

	// first instance stores the set
	reg1, err := New(Opts{Redis: redis})
	require.NoError(t, err)
	reg1.SetPubkeys([]string{testVouchPubkey})
	require.NoError(t, redis.SetVouchRegistry([]string{testVouchPubkey}))

	// second instance (simulating restart / another relay) recovers from Redis
	reg2, err := New(Opts{Redis: redis})
	require.NoError(t, err)
	require.True(t, reg2.IsVouchPubkey(testVouchPubkey))
	require.False(t, reg2.IsVouchPubkey(testExternalPubkey))

	// empty registry stored clears the set
	require.NoError(t, redis.SetVouchRegistry(nil))
	reg3, err := New(Opts{Redis: redis})
	require.NoError(t, err)
	require.False(t, reg3.IsVouchPubkey(testVouchPubkey))
}
