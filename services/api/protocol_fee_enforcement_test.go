package api

import (
	"net/http/httptest"
	"testing"
	"time"

	builderApiV1 "github.com/attestantio/go-builder-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/flashbots/go-boost-utils/utils"
	"github.com/flashbots/mev-boost-relay/common"
	"github.com/flashbots/mev-boost-relay/services/registry"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

const (
	testVFDAddress     = "0x9325008eE3B5982c10010C8f12b6CD4943F48fA6"
	testForeignAddress = "0x95222290DD7278Aa3Ddd389Cc1E1d165CC4BAfe5"
	testVouchPubkey    = "0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249"
	testExternalPubkey = "0xb872a4f5f596ea7dfd695e45afbe4551b405b10dafba98b2d897c58a5047fc288ef2c1bc4216f906ea05d7fdbed61116"
)

func newEnforcedBackend(t *testing.T) *testBackend {
	t.Helper()
	backend := newTestBackend(t, 1)
	vfd, err := utils.HexToAddress(testVFDAddress)
	require.NoError(t, err)
	reg, err := registry.New(registry.Opts{})
	require.NoError(t, err)
	reg.SetPubkeys([]string{testVouchPubkey})
	backend.relay.protocolFeeRecipient = &vfd
	backend.relay.vouchRegistry = reg
	backend.relay.proposerDutiesMap = make(map[uint64]*common.BuilderGetValidatorsResponseEntry)
	return backend
}

func TestCheckProtocolFeeRecipientPredicate(t *testing.T) {
	vfd, err := utils.HexToAddress(testVFDAddress)
	require.NoError(t, err)
	foreign, err := utils.HexToAddress(testForeignAddress)
	require.NoError(t, err)

	reg, err := registry.New(registry.Opts{})
	require.NoError(t, err)
	reg.SetPubkeys([]string{testVouchPubkey})

	enforced := &testBackend{}
	enforced.relay = &RelayAPI{protocolFeeRecipient: &vfd, vouchRegistry: reg}

	// registry-hit + VFD → allowed
	require.True(t, enforced.relay.checkProtocolFeeRecipient(testVouchPubkey, vfd))
	// registry-hit + foreign → rejected (fail-closed)
	require.False(t, enforced.relay.checkProtocolFeeRecipient(testVouchPubkey, foreign))
	// registry-miss + any recipient → allowed (fail-open, external validator)
	require.True(t, enforced.relay.checkProtocolFeeRecipient(testExternalPubkey, foreign))
	require.True(t, enforced.relay.checkProtocolFeeRecipient(testExternalPubkey, vfd))
	// case-insensitive pubkey lookup
	require.False(t, enforced.relay.checkProtocolFeeRecipient("0x8A1D7B8DD64E0AAFE7EA7B6C95065C9364CF99D38470C12EE807D55F7DE1529AD29CE2C422E0B65E3D5A05C02CACA249", foreign))

	// disabled (nil recipient/registry) → always allowed
	disabled := &RelayAPI{}
	require.True(t, disabled.checkProtocolFeeRecipient(testVouchPubkey, foreign))
}

func TestProtocolFeeEnforcementRegistrationJSON(t *testing.T) {
	backend := newEnforcedBackend(t)
	foreign, err := utils.HexToAddress(testForeignAddress)
	require.NoError(t, err)

	reg := &common.SimpleValidatorRegistration{
		Pubkey:       common.NewPubkeyHex(testVouchPubkey),
		FeeRecipient: foreign,
		GasLimit:     30000000,
		Timestamp:    time.Now().UTC(),
		Signature:    "0x00",
	}
	_, userErr, _ := backend.relay.processValidatorRegistrationJSON([]*common.SimpleValidatorRegistration{reg})
	require.ErrorIs(t, userErr, common.ErrFeeRecipientNotAllowed)
}

func TestProtocolFeeEnforcementRegistrationSSZ(t *testing.T) {
	backend := newEnforcedBackend(t)
	foreign, err := utils.HexToAddress(testForeignAddress)
	require.NoError(t, err)
	pk, err := utils.HexToPubkey(testVouchPubkey)
	require.NoError(t, err)

	signed := &builderApiV1.SignedValidatorRegistration{
		Message: &builderApiV1.ValidatorRegistration{
			Pubkey:       pk,
			FeeRecipient: foreign,
			GasLimit:     30000000,
			Timestamp:    time.Now().UTC(),
		},
		Signature: phase0.BLSSignature{},
	}
	_, userErr, _ := backend.relay.processValidatorRegistrationsSSZ([]*builderApiV1.SignedValidatorRegistration{signed})
	require.ErrorIs(t, userErr, common.ErrFeeRecipientNotAllowed)
}

func TestProtocolFeeEnforcementSubmissionGate(t *testing.T) {
	vfd, err := utils.HexToAddress(testVFDAddress)
	require.NoError(t, err)
	foreign, err := utils.HexToAddress(testForeignAddress)
	require.NoError(t, err)
	vouchPk, err := utils.HexToPubkey(testVouchPubkey)
	require.NoError(t, err)
	externalPk, err := utils.HexToPubkey(testExternalPubkey)
	require.NoError(t, err)

	cases := []struct {
		description string
		pubkey      phase0.BLSPubKey
		registered  bellatrix.ExecutionAddress
		expectOk    bool
	}{
		{
			description: "vouch validator registered VFD → allowed",
			pubkey:      vouchPk,
			registered:  vfd,
			expectOk:    true,
		},
		{
			description: "vouch validator registered foreign recipient → rejected (the lock)",
			pubkey:      vouchPk,
			registered:  foreign,
			expectOk:    false,
		},
		{
			description: "external validator any recipient → allowed",
			pubkey:      externalPk,
			registered:  foreign,
			expectOk:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.description, func(t *testing.T) {
			backend := newEnforcedBackend(t)
			backend.relay.proposerDutiesMap[testSlot] = &common.BuilderGetValidatorsResponseEntry{
				Entry: &builderApiV1.SignedValidatorRegistration{
					Message: &builderApiV1.ValidatorRegistration{
						Pubkey:       tc.pubkey,
						FeeRecipient: tc.registered,
						GasLimit:     testGasLimit,
					},
				},
			}

			w := httptest.NewRecorder()
			log := logrus.NewEntry(logrus.New())
			bidTrace := &builderApiV1.BidTrace{
				Slot:                 testSlot,
				ProposerFeeRecipient: tc.registered,
			}
			_, ok := backend.relay.checkSubmissionFeeRecipient(w, log, bidTrace)
			require.Equal(t, tc.expectOk, ok)
		})
	}
}

func TestProtocolFeeEnforcementDisabledDefault(t *testing.T) {
	// Default backend has no protocol fee recipient / registry → stock behavior.
	backend := newTestBackend(t, 1)
	require.Nil(t, backend.relay.protocolFeeRecipient)
	require.Nil(t, backend.relay.vouchRegistry)

	foreign, err := utils.HexToAddress(testForeignAddress)
	require.NoError(t, err)

	// registration with any recipient is processed (fails later on unknown validator,
	// not on the fee-recipient predicate)
	reg := &common.SimpleValidatorRegistration{
		Pubkey:       common.NewPubkeyHex(testVouchPubkey),
		FeeRecipient: foreign,
		GasLimit:     30000000,
		Timestamp:    time.Now().UTC(),
		Signature:    "0x00",
	}
	_, userErr, _ := backend.relay.processValidatorRegistrationJSON([]*common.SimpleValidatorRegistration{reg})
	require.NotErrorIs(t, userErr, common.ErrFeeRecipientNotAllowed)

	// submission gate with a foreign registered recipient passes the predicate
	backend.relay.proposerDutiesMap = make(map[uint64]*common.BuilderGetValidatorsResponseEntry)
	vouchPk, err := utils.HexToPubkey(testVouchPubkey)
	require.NoError(t, err)
	backend.relay.proposerDutiesMap[testSlot] = &common.BuilderGetValidatorsResponseEntry{
		Entry: &builderApiV1.SignedValidatorRegistration{
			Message: &builderApiV1.ValidatorRegistration{
				Pubkey:       vouchPk,
				FeeRecipient: foreign,
				GasLimit:     testGasLimit,
			},
		},
	}
	w := httptest.NewRecorder()
	log := logrus.NewEntry(logrus.New())
	bidTrace := &builderApiV1.BidTrace{Slot: testSlot, ProposerFeeRecipient: foreign}
	_, ok := backend.relay.checkSubmissionFeeRecipient(w, log, bidTrace)
	require.True(t, ok)
}
