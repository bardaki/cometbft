package blocksync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/tendermint/tendermint/internal/consensus"
	"github.com/tendermint/tendermint/internal/eventbus"
	"github.com/tendermint/tendermint/internal/p2p"
	"github.com/tendermint/tendermint/internal/p2p/conn"
	sm "github.com/tendermint/tendermint/internal/state"
	"github.com/tendermint/tendermint/internal/store"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/libs/service"
	"github.com/tendermint/tendermint/light"
	bcproto "github.com/tendermint/tendermint/proto/tendermint/blocksync"
	"github.com/tendermint/tendermint/types"
)

var _ service.Service = (*Reactor)(nil)

const (
	// BlockSyncChannel is a channel for blocks and status updates
	BlockSyncChannel = p2p.ChannelID(0x40)

	trySyncIntervalMS = 10

	// ask for best height every 10s
	statusUpdateIntervalSeconds = 10

	// check if we should switch to consensus reactor
	switchToConsensusIntervalSeconds = 1

	// switch to consensus after this duration of inactivity
	syncTimeout = 60 * time.Second
)

func GetChannelDescriptor() *p2p.ChannelDescriptor {
	return &p2p.ChannelDescriptor{
		ID:                  BlockSyncChannel,
		MessageType:         new(bcproto.Message),
		Priority:            5,
		SendQueueCapacity:   1000,
		RecvBufferCapacity:  1024,
		RecvMessageCapacity: MaxMsgSize,
		Name:                "blockSync",
	}
}

type consensusReactor interface {
	// For when we switch from block sync reactor to the consensus
	// machine.
	SwitchToConsensus(ctx context.Context, state sm.State, skipWAL bool)
}

type peerError struct {
	err    error
	peerID types.NodeID
}

func (e peerError) Error() string {
	return fmt.Sprintf("error with peer %v: %s", e.peerID, e.err.Error())
}

func (r *Reactor) VerifyAdjacent(
	trustedHeader *types.SignedHeader, // height=X
	untrustedHeader *types.SignedHeader, // height=X+1
	untrustedVals *types.ValidatorSet, // height=X+1)
) error {

	if len(trustedHeader.NextValidatorsHash) == 0 {
		return errors.New("next validators hash in trusted header is empty")
	}

	if untrustedHeader.Height != trustedHeader.Height+1 {
		return errors.New("headers must be adjacent in height")
	}

	if err := untrustedHeader.ValidateBasic(trustedHeader.ChainID); err != nil {
		return fmt.Errorf("untrustedHeader.ValidateBasic failed: %w", err)
	}

	if untrustedHeader.Height <= trustedHeader.Height {
		return fmt.Errorf("expected new header height %d to be greater than one of old header %d",
			untrustedHeader.Height,
			trustedHeader.Height)
	}

	if !untrustedHeader.Time.After(trustedHeader.Time) {
		return fmt.Errorf("expected new header time %v to be after old header time %v",
			untrustedHeader.Time,
			trustedHeader.Time)
	}

	if !bytes.Equal(untrustedHeader.ValidatorsHash, untrustedVals.Hash()) {
		return fmt.Errorf("expected new header validators (%X) to match those that were supplied (%X) at height %d",
			untrustedHeader.ValidatorsHash,
			untrustedVals.Hash(),
			untrustedHeader.Height,
		)
	}

	// Check the validator hashes are the same
	if !bytes.Equal(untrustedHeader.ValidatorsHash, trustedHeader.NextValidatorsHash) {
		err := fmt.Errorf("expected old header's next validators (%X) to match those from new header (%X)",
			trustedHeader.NextValidatorsHash,
			untrustedHeader.ValidatorsHash,
		)
		return light.ErrInvalidHeader{Reason: err}
	}
	return nil
}

// Reactor handles long-term catchup syncing.
type Reactor struct {
	service.BaseService
	logger log.Logger

	// immutable
	initialState sm.State
	// store
	stateStore sm.Store

	blockExec   *sm.BlockExecutor
	store       *store.BlockStore
	pool        *BlockPool
	consReactor consensusReactor
	blockSync   *atomicBool

	chCreator  p2p.ChannelCreator
	peerEvents p2p.PeerEventSubscriber

	requestsCh <-chan BlockRequest
	errorsCh   <-chan peerError

	metrics  *consensus.Metrics
	eventBus *eventbus.EventBus

	syncStartTime time.Time

	lastTrustedBlock *BlockResponse
}

// NewReactor returns new reactor instance.
func NewReactor(
	logger log.Logger,
	stateStore sm.Store,
	blockExec *sm.BlockExecutor,
	store *store.BlockStore,
	consReactor consensusReactor,
	channelCreator p2p.ChannelCreator,
	peerEvents p2p.PeerEventSubscriber,
	blockSync bool,
	metrics *consensus.Metrics,
	eventBus *eventbus.EventBus,
) *Reactor {
	r := &Reactor{
		logger:           logger,
		stateStore:       stateStore,
		blockExec:        blockExec,
		store:            store,
		consReactor:      consReactor,
		blockSync:        newAtomicBool(blockSync),
		chCreator:        channelCreator,
		peerEvents:       peerEvents,
		metrics:          metrics,
		eventBus:         eventBus,
		lastTrustedBlock: nil,
	}

	r.BaseService = *service.NewBaseService(logger, "BlockSync", r)
	return r
}

// OnStart starts separate go routines for each p2p Channel and listens for
// envelopes on each. In addition, it also listens for peer updates and handles
// messages on that p2p channel accordingly. The caller must be sure to execute
// OnStop to ensure the outbound p2p Channels are closed.
//
// If blockSync is enabled, we also start the pool and the pool processing
// goroutine. If the pool fails to start, an error is returned.
func (r *Reactor) OnStart(ctx context.Context) error {
	blockSyncCh, err := r.chCreator(ctx, GetChannelDescriptor())
	if err != nil {
		return err
	}
	r.chCreator = func(context.Context, *conn.ChannelDescriptor) (*p2p.Channel, error) { return blockSyncCh, nil }

	state, err := r.stateStore.Load()
	if err != nil {
		return err
	}
	r.initialState = state

	if state.LastBlockHeight != r.store.Height() {
		return fmt.Errorf("state (%v) and store (%v) height mismatch", state.LastBlockHeight, r.store.Height())
	}

	startHeight := r.store.Height() + 1
	if startHeight == 1 {
		startHeight = state.InitialHeight
	}

	requestsCh := make(chan BlockRequest, maxTotalRequesters)
	errorsCh := make(chan peerError, maxPeerErrBuffer) // NOTE: The capacity should be larger than the peer count.
	r.pool = NewBlockPool(r.logger, startHeight, requestsCh, errorsCh)
	r.requestsCh = requestsCh
	r.errorsCh = errorsCh

	if r.blockSync.IsSet() {
		if err := r.pool.Start(ctx); err != nil {
			return err
		}
		go r.requestRoutine(ctx, blockSyncCh)

		go r.poolRoutine(ctx, false, blockSyncCh)
	}

	go r.processBlockSyncCh(ctx, blockSyncCh)
	go r.processPeerUpdates(ctx, r.peerEvents(ctx), blockSyncCh)

	return nil
}

// OnStop stops the reactor by signaling to all spawned goroutines to exit and
// blocking until they all exit.
func (r *Reactor) OnStop() {
	if r.blockSync.IsSet() {
		r.pool.Stop()
	}
}

// respondToPeer loads a block and sends it to the requesting peer, if we have it.
// Otherwise, we'll respond saying we do not have it.
func (r *Reactor) respondToPeer(ctx context.Context, msg *bcproto.BlockRequest, peerID types.NodeID, blockSyncCh *p2p.Channel) error {
	block := r.store.LoadBlockProto(msg.Height)
	if block != nil {
		blockCommit := r.store.LoadBlockCommitProto(msg.Height)
		if blockCommit != nil {
			return blockSyncCh.Send(ctx, p2p.Envelope{
				To:      peerID,
				Message: &bcproto.BlockResponse{Block: block},
			})
		}
	}

	r.logger.Info("peer requesting a block we do not have", "peer", peerID, "height", msg.Height)

	return blockSyncCh.Send(ctx, p2p.Envelope{
		To:      peerID,
		Message: &bcproto.NoBlockResponse{Height: msg.Height},
	})
}

// handleMessage handles an Envelope sent from a peer on a specific p2p Channel.
// It will handle errors and any possible panics gracefully. A caller can handle
// any error returned by sending a PeerError on the respective channel.
func (r *Reactor) handleMessage(ctx context.Context, chID p2p.ChannelID, envelope *p2p.Envelope, blockSyncCh *p2p.Channel) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("panic in processing message: %v", e)
			r.logger.Error(
				"recovering from processing message panic",
				"err", err,
				"stack", string(debug.Stack()),
			)
		}
	}()

	r.logger.Debug("received message", "message", envelope.Message, "peer", envelope.From)

	switch chID {
	case BlockSyncChannel:
		switch msg := envelope.Message.(type) {
		case *bcproto.BlockRequest:
			return r.respondToPeer(ctx, msg, envelope.From, blockSyncCh)
		case *bcproto.BlockResponse:
			block, err := types.BlockFromProto(msg.Block)
			if err != nil {
				r.logger.Error("failed to convert block from proto", "err", err)
				return err
			}

			r.pool.AddBlock(envelope.From, block, block.Size())
		case *bcproto.StatusRequest:
			return blockSyncCh.Send(ctx, p2p.Envelope{
				To: envelope.From,
				Message: &bcproto.StatusResponse{
					Height: r.store.Height(),
					Base:   r.store.Base(),
				},
			})
		case *bcproto.StatusResponse:
			r.pool.SetPeerRange(envelope.From, msg.Base, msg.Height)

		case *bcproto.NoBlockResponse:
			r.logger.Debug("peer does not have the requested block",
				"peer", envelope.From,
				"height", msg.Height)

		default:
			return fmt.Errorf("received unknown message: %T", msg)
		}

	default:
		err = fmt.Errorf("unknown channel ID (%d) for envelope (%v)", chID, envelope)
	}

	return err
}

// processBlockSyncCh initiates a blocking process where we listen for and handle
// envelopes on the BlockSyncChannel and blockSyncOutBridgeCh. Any error encountered during
// message execution will result in a PeerError being sent on the BlockSyncChannel.
// When the reactor is stopped, we will catch the signal and close the p2p Channel
// gracefully.
func (r *Reactor) processBlockSyncCh(ctx context.Context, blockSyncCh *p2p.Channel) {
	iter := blockSyncCh.Receive(ctx)
	for iter.Next(ctx) {
		envelope := iter.Envelope()
		if err := r.handleMessage(ctx, blockSyncCh.ID, envelope, blockSyncCh); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}

			r.logger.Error("failed to process message", "ch_id", blockSyncCh.ID, "envelope", envelope, "err", err)
			if serr := blockSyncCh.SendError(ctx, p2p.PeerError{
				NodeID: envelope.From,
				Err:    err,
			}); serr != nil {
				return
			}
		}
	}
}

// processPeerUpdate processes a PeerUpdate.
func (r *Reactor) processPeerUpdate(ctx context.Context, peerUpdate p2p.PeerUpdate, blockSyncCh *p2p.Channel) {
	r.logger.Debug("received peer update", "peer", peerUpdate.NodeID, "status", peerUpdate.Status)

	// XXX: Pool#RedoRequest can sometimes give us an empty peer.
	if len(peerUpdate.NodeID) == 0 {
		return
	}

	switch peerUpdate.Status {
	case p2p.PeerStatusUp:
		// send a status update the newly added peer
		if err := blockSyncCh.Send(ctx, p2p.Envelope{
			To: peerUpdate.NodeID,
			Message: &bcproto.StatusResponse{
				Base:   r.store.Base(),
				Height: r.store.Height(),
			},
		}); err != nil {
			r.pool.RemovePeer(peerUpdate.NodeID)
			if err := blockSyncCh.SendError(ctx, p2p.PeerError{
				NodeID: peerUpdate.NodeID,
				Err:    err,
			}); err != nil {
				return
			}
		}

	case p2p.PeerStatusDown:
		r.pool.RemovePeer(peerUpdate.NodeID)
	}
}

// processPeerUpdates initiates a blocking process where we listen for and handle
// PeerUpdate messages. When the reactor is stopped, we will catch the signal and
// close the p2p PeerUpdatesCh gracefully.
func (r *Reactor) processPeerUpdates(ctx context.Context, peerUpdates *p2p.PeerUpdates, blockSyncCh *p2p.Channel) {
	for {
		select {
		case <-ctx.Done():
			return
		case peerUpdate := <-peerUpdates.Updates():
			r.processPeerUpdate(ctx, peerUpdate, blockSyncCh)
		}
	}
}

// SwitchToBlockSync is called by the state sync reactor when switching to fast
// sync.
func (r *Reactor) SwitchToBlockSync(ctx context.Context, state sm.State) error {
	r.blockSync.Set()
	r.initialState = state
	r.pool.height = state.LastBlockHeight + 1

	if err := r.pool.Start(ctx); err != nil {
		return err
	}

	r.syncStartTime = time.Now()

	bsCh, err := r.chCreator(ctx, GetChannelDescriptor())
	if err != nil {
		return err
	}

	go r.requestRoutine(ctx, bsCh)
	go r.poolRoutine(ctx, true, bsCh)

	return nil
}

func (r *Reactor) requestRoutine(ctx context.Context, blockSyncCh *p2p.Channel) {
	statusUpdateTicker := time.NewTicker(statusUpdateIntervalSeconds * time.Second)
	defer statusUpdateTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case request := <-r.requestsCh:
			if err := blockSyncCh.Send(ctx, p2p.Envelope{
				To:      request.PeerID,
				Message: &bcproto.BlockRequest{Height: request.Height},
			}); err != nil {
				if err := blockSyncCh.SendError(ctx, p2p.PeerError{
					NodeID: request.PeerID,
					Err:    err,
				}); err != nil {
					return
				}
			}
		case pErr := <-r.errorsCh:
			if err := blockSyncCh.SendError(ctx, p2p.PeerError{
				NodeID: pErr.peerID,
				Err:    pErr.err,
			}); err != nil {
				return
			}
		case <-statusUpdateTicker.C:
			if err := blockSyncCh.Send(ctx, p2p.Envelope{
				Broadcast: true,
				Message:   &bcproto.StatusRequest{},
			}); err != nil {
				return
			}
		}
	}
}

// poolRoutine handles messages from the poolReactor telling the reactor what to
// do.
//
// NOTE: Don't sleep in the FOR_LOOP or otherwise slow it down!
func (r *Reactor) poolRoutine(ctx context.Context, stateSynced bool, blockSyncCh *p2p.Channel) {
	var (
		trySyncTicker           = time.NewTicker(trySyncIntervalMS * time.Millisecond)
		switchToConsensusTicker = time.NewTicker(switchToConsensusIntervalSeconds * time.Second)

		blocksSynced = uint64(0)
		state        = r.initialState

		lastHundred = time.Now()
		lastRate    = 0.0

		didProcessCh = make(chan struct{}, 1)
	)

	defer trySyncTicker.Stop()
	defer switchToConsensusTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-switchToConsensusTicker.C:
			var (
				height, numPending, lenRequesters = r.pool.GetStatus()
				lastAdvance                       = r.pool.LastAdvance()
			)

			r.logger.Debug(
				"consensus ticker",
				"num_pending", numPending,
				"total", lenRequesters,
				"height", height,
			)

			switch {
			case r.pool.IsCaughtUp():
				r.logger.Info("switching to consensus reactor", "height", height)

			case time.Since(lastAdvance) > syncTimeout:
				r.logger.Error("no progress since last advance", "last_advance", lastAdvance)

			default:
				r.logger.Info(
					"not caught up yet",
					"height", height,
					"max_peer_height", r.pool.MaxPeerHeight(),
					"timeout_in", syncTimeout-time.Since(lastAdvance),
				)
				continue
			}

			r.pool.Stop()

			r.blockSync.UnSet()

			if r.consReactor != nil {
				r.consReactor.SwitchToConsensus(ctx, state, blocksSynced > 0 || stateSynced)
			}

			return

		case <-trySyncTicker.C:
			select {
			case didProcessCh <- struct{}{}:
			default:
			}
		case <-didProcessCh:
			// NOTE: It is a subtle mistake to process more than a single block at a
			// time (e.g. 10) here, because we only send one BlockRequest per loop
			// iteration. The ratio mismatch can result in starving of blocks, i.e. a
			// sudden burst of requests and responses, and repeat. Consequently, it is
			// better to split these routines rather than coupling them as it is
			// written here.
			//
			// TODO: Uncouple from request routine.

			newBlock, verifyBlock := r.pool.PeekTwoBlocks()

			if newBlock == nil || verifyBlock == nil {
				continue
			} else {
				didProcessCh <- struct{}{}
			}

			newBlockParts, err2 := newBlock.MakePartSet(types.BlockPartSizeBytes)
			if err2 != nil {
				r.logger.Error("failed to make ",
					"height", newBlock.Height,
					"err", err2.Error())
				return
			}

			var (
				newBlockPartSetHeader = newBlockParts.Header()
				newBlockID            = types.BlockID{Hash: newBlock.Hash(), PartSetHeader: newBlockPartSetHeader}
			)

			// ToDo @jmalicevic
			// newBlock.last commit - validates r.lastTrustedBlock
			// if fail - peer dismissed
			// r.lastTrustedBlock.header. nextValidator = newBlock. validator
			// validators. validate (newBlock)
			if r.lastTrustedBlock != nil {

				// If the blockID in LastCommit of NewBlock does not match the trusted block
				// we can assume NewBlock is not correct
				if !(newBlock.LastCommit.BlockID.Equals(r.lastTrustedBlock.commit.BlockID)) {
					peerID := r.pool.RedoRequest(r.lastTrustedBlock.block.Height + 1)
					if serr := blockSyncCh.SendError(ctx, p2p.PeerError{
						NodeID: peerID,
						Err:    errors.New("invalid block for verification"),
					}); serr != nil {
						return
					}
				}

				// Todo: Verify verifyBlock.LastCommit validators against state.NextValidators
				// If they do not match, need a new verifyBlock

				if err := state.NextValidators.VerifyCommitLight(state.ChainID, newBlockID, newBlock.Height, verifyBlock.LastCommit); err != nil {

					err = fmt.Errorf("invalid verification block, validator hash does not match %w", err)
					r.logger.Error(
						err.Error(),
						"last_commit", verifyBlock.LastCommit,
						"block_id", newBlockID,
						"height", r.lastTrustedBlock.block.Height,
					)
					peerID := r.pool.RedoRequest(r.lastTrustedBlock.block.Height + 2)
					if serr := blockSyncCh.SendError(ctx, p2p.PeerError{
						NodeID: peerID,
						Err:    err,
					}); serr != nil {
						return
					}
				}
				// Verify NewBlock usign the validator set obtained after applying the last block
				// Note: VerifyAdjacent in the LightClient relies on a trusting period which is not applicable here
				// ToDo: We need witness verification here as well
				err := r.VerifyAdjacent(&types.SignedHeader{Header: &r.lastTrustedBlock.block.Header, Commit: r.lastTrustedBlock.commit}, &types.SignedHeader{Header: &newBlock.Header, Commit: verifyBlock.LastCommit}, state.NextValidators)
				if err != nil {
					err = fmt.Errorf("invalid last commit: %w", err)
					r.logger.Error(
						err.Error(),
						"last_commit", verifyBlock.LastCommit,
						"block_id", newBlockID,
						"height", r.lastTrustedBlock.block.Height,
					)

					peerID := r.pool.RedoRequest(r.lastTrustedBlock.block.Height + 1)
					if serr := blockSyncCh.SendError(ctx, p2p.PeerError{
						NodeID: peerID,
						Err:    err,
					}); serr != nil {
						return
					}
				}
			} else {

				if r.initialState.LastBlockHeight != 0 {
					r.lastTrustedBlock = &BlockResponse{r.store.LoadBlock(r.initialState.LastBlockHeight), r.store.LoadBlockCommit(r.initialState.LastBlockHeight)}
					if r.lastTrustedBlock == nil {
						panic("Failed to load last trusted block")
					}
				}
				// chainID := r.initialState.ChainID
				oldHash := r.initialState.Validators.Hash()
				if !bytes.Equal(oldHash, newBlock.ValidatorsHash) {

					fmt.Println(

						"initial hash ", r.initialState.Validators.Hash(),
						"new hash ", newBlock.ValidatorsHash,
					)
					return
				}
				r.lastTrustedBlock = &BlockResponse{block: newBlock, commit: verifyBlock.LastCommit}
			}
			r.lastTrustedBlock.block = newBlock
			r.lastTrustedBlock.commit = verifyBlock.LastCommit
			r.pool.PopRequest()

			// TODO: batch saves so we do not persist to disk every block
			r.store.SaveBlock(newBlock, newBlockParts, verifyBlock.LastCommit)

			var err error

			// TODO: Same thing for app - but we would need a way to get the hash
			// without persisting the state.
			state, err = r.blockExec.ApplyBlock(ctx, state, newBlockID, newBlock)
			if err != nil {
				// TODO: This is bad, are we zombie?
				panic(fmt.Sprintf("failed to process committed block (%d:%X): %v", newBlock.Height, newBlock.Hash(), err))
			}

			r.metrics.RecordConsMetrics(newBlock)

			blocksSynced++

			if blocksSynced%100 == 0 {
				lastRate = 0.9*lastRate + 0.1*(100/time.Since(lastHundred).Seconds())
				r.logger.Info(
					"block sync rate",
					"height", r.pool.height,
					"max_peer_height", r.pool.MaxPeerHeight(),
					"blocks/s", lastRate,
				)

				lastHundred = time.Now()
			}

		}
	}
}

func (r *Reactor) GetMaxPeerBlockHeight() int64 {
	return r.pool.MaxPeerHeight()
}

func (r *Reactor) GetTotalSyncedTime() time.Duration {
	if !r.blockSync.IsSet() || r.syncStartTime.IsZero() {
		return time.Duration(0)
	}
	return time.Since(r.syncStartTime)
}

func (r *Reactor) GetRemainingSyncTime() time.Duration {
	if !r.blockSync.IsSet() {
		return time.Duration(0)
	}

	targetSyncs := r.pool.targetSyncBlocks()
	currentSyncs := r.store.Height() - r.pool.startHeight + 1
	lastSyncRate := r.pool.getLastSyncRate()
	if currentSyncs < 0 || lastSyncRate < 0.001 {
		return time.Duration(0)
	}

	remain := float64(targetSyncs-currentSyncs) / lastSyncRate

	return time.Duration(int64(remain * float64(time.Second)))
}

func (r *Reactor) PublishStatus(ctx context.Context, event types.EventDataBlockSyncStatus) error {
	if r.eventBus == nil {
		return errors.New("event bus is not configured")
	}
	return r.eventBus.PublishEventBlockSyncStatus(ctx, event)
}

// atomicBool is an atomic Boolean, safe for concurrent use by multiple
// goroutines.
type atomicBool int32

// newAtomicBool creates an atomicBool with given initial value.
func newAtomicBool(ok bool) *atomicBool {
	ab := new(atomicBool)
	if ok {
		ab.Set()
	}
	return ab
}

// Set sets the Boolean to true.
func (ab *atomicBool) Set() { atomic.StoreInt32((*int32)(ab), 1) }

// UnSet sets the Boolean to false.
func (ab *atomicBool) UnSet() { atomic.StoreInt32((*int32)(ab), 0) }

// IsSet returns whether the Boolean is true.
func (ab *atomicBool) IsSet() bool { return atomic.LoadInt32((*int32)(ab))&1 == 1 }