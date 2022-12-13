package validator

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prysmaticlabs/prysm/v3/beacon-chain/core/blocks"
	"github.com/prysmaticlabs/prysm/v3/beacon-chain/core/signing"
	"github.com/prysmaticlabs/prysm/v3/beacon-chain/db/kv"
	"github.com/prysmaticlabs/prysm/v3/config/params"
	consensusblocks "github.com/prysmaticlabs/prysm/v3/consensus-types/blocks"
	coreBlock "github.com/prysmaticlabs/prysm/v3/consensus-types/blocks"
	"github.com/prysmaticlabs/prysm/v3/consensus-types/interfaces"
	types "github.com/prysmaticlabs/prysm/v3/consensus-types/primitives"
	"github.com/prysmaticlabs/prysm/v3/encoding/bytesutil"
	"github.com/prysmaticlabs/prysm/v3/encoding/ssz"
	enginev1 "github.com/prysmaticlabs/prysm/v3/proto/engine/v1"
	ethpb "github.com/prysmaticlabs/prysm/v3/proto/prysm/v1alpha1"
	"github.com/prysmaticlabs/prysm/v3/runtime/version"
	"github.com/prysmaticlabs/prysm/v3/time/slots"
	"github.com/sirupsen/logrus"
)

// builderGetPayloadMissCount tracks the number of misses when validator tries to get a payload from builder
var builderGetPayloadMissCount = promauto.NewCounter(prometheus.CounterOpts{
	Name: "builder_get_payload_miss_count",
	Help: "The number of get payload misses for validator requests to builder",
})

// blockBuilderTimeout is the maximum amount of time allowed for a block builder to respond to a
// block request. This value is known as `BUILDER_PROPOSAL_DELAY_TOLERANCE` in builder spec.
const blockBuilderTimeout = 1 * time.Second

func (vs *Server) canUseBuilder(ctx context.Context, slot types.Slot, idx types.ValidatorIndex) (bool, error) {
	registered, err := vs.validatorRegistered(ctx, idx)
	if err != nil {
		return false, err
	}
	if !registered {
		return false, nil
	}
	return vs.circuitBreakBuilder(slot)
}

// This function retrieves the payload header given the slot number and the validator index.
// It's a no-op if the latest head block is not versioned bellatrix.
func (vs *Server) getPayloadHeaderFromBuilder(ctx context.Context, slot types.Slot, idx types.ValidatorIndex) (interfaces.ExecutionData, error) {
	b, err := vs.HeadFetcher.HeadBlock(ctx)
	if err != nil {
		return nil, err
	}
	if blocks.IsPreBellatrixVersion(b.Version()) {
		return nil, nil
	}

	h, err := b.Block().Body().Execution()
	if err != nil {
		return nil, err
	}
	pk, err := vs.HeadFetcher.HeadValidatorIndexToPublicKey(ctx, idx)
	if err != nil {
		return nil, err
	}
	bid, err := vs.BlockBuilder.GetHeader(ctx, slot, bytesutil.ToBytes32(h.BlockHash()), pk)
	if err != nil {
		return nil, err
	}
	if bid == nil || bid.Message == nil {
		return nil, errors.New("builder returned nil bid")
	}

	v := new(big.Int).SetBytes(bytesutil.ReverseByteOrder(bid.Message.Value))
	if v.String() == "0" {
		return nil, errors.New("builder returned header with 0 bid amount")
	}

	emptyRoot, err := ssz.TransactionsRoot([][]byte{})
	if err != nil {
		return nil, err
	}

	if bytesutil.ToBytes32(bid.Message.Header.TransactionsRoot) == emptyRoot {
		return nil, errors.New("builder returned header with an empty tx root")
	}

	if !bytes.Equal(bid.Message.Header.ParentHash, h.BlockHash()) {
		return nil, fmt.Errorf("incorrect parent hash %#x != %#x", bid.Message.Header.ParentHash, h.BlockHash())
	}

	t, err := slots.ToTime(uint64(vs.TimeFetcher.GenesisTime().Unix()), slot)
	if err != nil {
		return nil, err
	}
	if bid.Message.Header.Timestamp != uint64(t.Unix()) {
		return nil, fmt.Errorf("incorrect timestamp %d != %d", bid.Message.Header.Timestamp, uint64(t.Unix()))
	}

	if err := vs.validateBuilderSignature(bid); err != nil {
		return nil, errors.Wrap(err, "could not validate builder signature")
	}

	log.WithFields(logrus.Fields{
		"value":         v.String(),
		"builderPubKey": fmt.Sprintf("%#x", bid.Message.Pubkey),
		"blockHash":     fmt.Sprintf("%#x", bid.Message.Header.BlockHash),
	}).Info("Received header with bid")

	return coreBlock.WrappedExecutionPayloadHeader(bid.Message.Header)
}

// This function retrieves the full payload block using the input blind block. This input must be versioned as
// bellatrix blind block. The output block will contain the full payload. The original header block
// will be returned the block builder is not configured.
func (vs *Server) unblindBuilderBlock(ctx context.Context, b interfaces.SignedBeaconBlock) (interfaces.SignedBeaconBlock, error) {
	if err := coreBlock.BeaconBlockIsNil(b); err != nil {
		return nil, err
	}

	// No-op if the input block is not version blind and bellatrix.
	if b.Version() != version.Bellatrix || !b.IsBlinded() {
		return b, nil
	}
	// No-op nothing if the builder has not been configured.
	if !vs.BlockBuilder.Configured() {
		return b, nil
	}

	agg, err := b.Block().Body().SyncAggregate()
	if err != nil {
		return nil, err
	}
	h, err := b.Block().Body().Execution()
	if err != nil {
		return nil, err
	}
	header, ok := h.Proto().(*enginev1.ExecutionPayloadHeader)
	if !ok {
		return nil, errors.New("execution data must be execution payload header")
	}
	parentRoot := b.Block().ParentRoot()
	stateRoot := b.Block().StateRoot()
	randaoReveal := b.Block().Body().RandaoReveal()
	graffiti := b.Block().Body().Graffiti()
	sig := b.Signature()
	sb := &ethpb.SignedBlindedBeaconBlockBellatrix{
		Block: &ethpb.BlindedBeaconBlockBellatrix{
			Slot:          b.Block().Slot(),
			ProposerIndex: b.Block().ProposerIndex(),
			ParentRoot:    parentRoot[:],
			StateRoot:     stateRoot[:],
			Body: &ethpb.BlindedBeaconBlockBodyBellatrix{
				RandaoReveal:           randaoReveal[:],
				Eth1Data:               b.Block().Body().Eth1Data(),
				Graffiti:               graffiti[:],
				ProposerSlashings:      b.Block().Body().ProposerSlashings(),
				AttesterSlashings:      b.Block().Body().AttesterSlashings(),
				Attestations:           b.Block().Body().Attestations(),
				Deposits:               b.Block().Body().Deposits(),
				VoluntaryExits:         b.Block().Body().VoluntaryExits(),
				SyncAggregate:          agg,
				ExecutionPayloadHeader: header,
			},
		},
		Signature: sig[:],
	}

	payload, err := vs.BlockBuilder.SubmitBlindedBlock(ctx, sb)
	if err != nil {
		return nil, err
	}
	headerRoot, err := header.HashTreeRoot()
	if err != nil {
		return nil, err
	}

	payloadRoot, err := payload.HashTreeRoot()
	if err != nil {
		return nil, err
	}
	if headerRoot != payloadRoot {
		return nil, fmt.Errorf("header and payload root do not match, consider disconnect from relay to avoid further issues, "+
			"%#x != %#x", headerRoot, payloadRoot)
	}

	bb := &ethpb.SignedBeaconBlockBellatrix{
		Block: &ethpb.BeaconBlockBellatrix{
			Slot:          sb.Block.Slot,
			ProposerIndex: sb.Block.ProposerIndex,
			ParentRoot:    sb.Block.ParentRoot,
			StateRoot:     sb.Block.StateRoot,
			Body: &ethpb.BeaconBlockBodyBellatrix{
				RandaoReveal:      sb.Block.Body.RandaoReveal,
				Eth1Data:          sb.Block.Body.Eth1Data,
				Graffiti:          sb.Block.Body.Graffiti,
				ProposerSlashings: sb.Block.Body.ProposerSlashings,
				AttesterSlashings: sb.Block.Body.AttesterSlashings,
				Attestations:      sb.Block.Body.Attestations,
				Deposits:          sb.Block.Body.Deposits,
				VoluntaryExits:    sb.Block.Body.VoluntaryExits,
				SyncAggregate:     agg,
				ExecutionPayload:  payload,
			},
		},
		Signature: sb.Signature,
	}
	wb, err := consensusblocks.NewSignedBeaconBlock(bb)
	if err != nil {
		return nil, err
	}

	log.WithFields(logrus.Fields{
		"blockHash":    fmt.Sprintf("%#x", h.BlockHash()),
		"feeRecipient": fmt.Sprintf("%#x", h.FeeRecipient()),
		"gasUsed":      h.GasUsed,
		"slot":         b.Block().Slot(),
		"txs":          len(payload.Transactions),
	}).Info("Retrieved full payload from builder")

	return wb, nil
}

// circuitBreakBuilder returns true if the builder is not allowed to be used due to circuit breaker conditions.
func (vs *Server) circuitBreakBuilder(s types.Slot) (bool, error) {
	if vs.ForkFetcher == nil || vs.ForkFetcher.ForkChoicer() == nil {
		return true, errors.New("no fork choicer configured")
	}

	// Circuit breaker is active if the missing consecutive slots greater than `MaxBuilderConsecutiveMissedSlots`.
	highestReceivedSlot := vs.ForkFetcher.ForkChoicer().HighestReceivedBlockSlot()
	maxConsecutiveSkipSlotsAllowed := params.BeaconConfig().MaxBuilderConsecutiveMissedSlots
	diff, err := s.SafeSubSlot(highestReceivedSlot)
	if err != nil {
		return true, err
	}
	if diff > maxConsecutiveSkipSlotsAllowed {
		log.WithFields(logrus.Fields{
			"currentSlot":                    s,
			"highestReceivedSlot":            highestReceivedSlot,
			"maxConsecutiveSkipSlotsAllowed": maxConsecutiveSkipSlotsAllowed,
		}).Warn("Builder circuit breaker activated due to missing consecutive slot")
		return true, nil
	}

	// Not much reason to check missed slots epoch rolling window if input slot is less than epoch.
	if s < params.BeaconConfig().SlotsPerEpoch {
		return false, nil
	}

	// Circuit breaker is active if the missing slots per epoch (rolling window) greater than `MaxBuilderEpochMissedSlots`.
	receivedCount, err := vs.ForkFetcher.ForkChoicer().ReceivedBlocksLastEpoch()
	if err != nil {
		return true, err
	}
	maxEpochSkipSlotsAllowed := params.BeaconConfig().MaxBuilderEpochMissedSlots
	diff, err = params.BeaconConfig().SlotsPerEpoch.SafeSub(receivedCount)
	if err != nil {
		return true, err
	}
	if diff > maxEpochSkipSlotsAllowed {
		log.WithFields(logrus.Fields{
			"totalMissed":              diff,
			"maxEpochSkipSlotsAllowed": maxEpochSkipSlotsAllowed,
		}).Warn("Builder circuit breaker activated due to missing enough slots last epoch")
		return true, nil
	}

	return false, nil
}

// validatorRegistered returns true if validator with index `id` was previously registered in the database.
func (vs *Server) validatorRegistered(ctx context.Context, id types.ValidatorIndex) (bool, error) {
	if vs.BeaconDB == nil {
		return false, errors.New("nil beacon db")
	}
	_, err := vs.BeaconDB.RegistrationByValidatorID(ctx, id)
	switch {
	case errors.Is(err, kv.ErrNotFoundFeeRecipient):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// Validates builder signature and returns an error if the signature is invalid.
func (vs *Server) validateBuilderSignature(bid *ethpb.SignedBuilderBid) error {
	d, err := signing.ComputeDomain(params.BeaconConfig().DomainApplicationBuilder,
		nil, /* fork version */
		nil  /* genesis val root */)
	if err != nil {
		return err
	}
	if bid == nil || bid.Message == nil {
		return errors.New("nil builder bid")
	}
	return signing.VerifySigningRoot(bid.Message, bid.Message.Pubkey, bid.Signature, d)
}
