package adder

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ipfs/ipfs-cluster/api"
	"go.uber.org/multierr"

	cid "github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	peer "github.com/libp2p/go-libp2p-core/peer"
	rpc "github.com/libp2p/go-libp2p-gorpc"
)

// ErrBlockAdder is returned when adding a to multiple destinations
// block fails on all of them.
var ErrBlockAdder = errors.New("failed to put block on all destinations")

// BlockStreamer helps streaming nodes to multiple destinations, as long as
// one of them is still working.
type BlockStreamer struct {
	dests     []peer.ID
	rpcClient *rpc.Client
	blocks    <-chan api.NodeWithMeta

	ctx    context.Context
	cancel context.CancelFunc
	errMu  sync.Mutex
	err    error
}

// NewBlockStreamer creates a BlockStreamer given an rpc client, allocated
// peers and a channel on which the blocks to stream are received.
func NewBlockStreamer(ctx context.Context, rpcClient *rpc.Client, dests []peer.ID, blocks <-chan api.NodeWithMeta) *BlockStreamer {
	bsCtx, cancel := context.WithCancel(ctx)

	bs := BlockStreamer{
		ctx:       bsCtx,
		cancel:    cancel,
		dests:     dests,
		rpcClient: rpcClient,
		blocks:    blocks,
		err:       nil,
	}

	go bs.streamBlocks()
	return &bs
}

// Done returns a channel which gets closed when the BlockStreamer has
// finished.
func (bs *BlockStreamer) Done() <-chan struct{} {
	return bs.ctx.Done()
}

func (bs *BlockStreamer) setErr(err error) {
	bs.errMu.Lock()
	bs.err = err
	bs.errMu.Unlock()
}

// Err returns any errors that happened after the operation of the
// BlockStreamer, for example when blocks could not be put to all nodes.
func (bs *BlockStreamer) Err() error {
	bs.errMu.Lock()
	defer bs.errMu.Unlock()
	return bs.err
}

func (bs *BlockStreamer) streamBlocks() {
	defer bs.cancel()

	// Nothing should be sent on out.
	// We drain though
	out := make(chan struct{})
	go func() {
		for range out {
		}
	}()

	errs := bs.rpcClient.MultiStream(
		bs.ctx,
		bs.dests,
		"IPFSConnector",
		"BlockStream",
		bs.blocks,
		out,
	)

	// this eliminates any nil errors.
	combinedErrors := multierr.Combine(errs...)
	if len(multierr.Errors(combinedErrors)) == len(bs.dests) {
		logger.Error(combinedErrors)
		bs.setErr(ErrBlockAdder)
	} else {
		logger.Warning("there were errors streaming blocks, but at least one destination succeeded")
		logger.Warning(combinedErrors)
	}
}

// // Add puts an ipld node to the allocated destinations.
// func (ba *BlockAdder) Add(ctx context.Context, node ipld.Node) error {
// 	nodeSerial := ipldNodeToNodeWithMeta(node)

// 	ctxs, cancels := rpcutil.CtxsWithCancel(ctx, len(ba.dests))
// 	defer rpcutil.MultiCancel(cancels)

// 	logger.Debugf("block put %s to %s", nodeSerial.Cid, ba.dests)
// 	errs := ba.rpcClient.MultiCall(
// 		ctxs,
// 		ba.dests,
// 		"IPFSConnector",
// 		"BlockPut",
// 		nodeSerial,
// 		rpcutil.RPCDiscardReplies(len(ba.dests)),
// 	)

// 	var successfulDests []peer.ID
// 	numErrs := 0
// 	for i, e := range errs {
// 		if e != nil {
// 			logger.Errorf("BlockPut on %s: %s", ba.dests[i], e)
// 			numErrs++
// 		}

// 		// RPCErrors include server errors (wrong RPC methods), client
// 		// errors (creating, writing or reading streams) and
// 		// authorization errors, but not IPFS errors from a failed blockput
// 		// for example.
// 		if rpc.IsRPCError(e) {
// 			continue
// 		}
// 		successfulDests = append(successfulDests, ba.dests[i])
// 	}

// 	// If all requests resulted in errors, fail.
// 	// Successful dests will have members when no errors happened
// 	// or when an error happened but it was not an RPC error.
// 	// As long as BlockPut worked in 1 destination, we move on.
// 	if numErrs == len(ba.dests) || len(successfulDests) == 0 {
// 		return ErrBlockAdder
// 	}

// 	ba.dests = successfulDests
// 	return nil
// }

// // AddMany puts multiple ipld nodes to allocated destinations.
// func (ba *BlockAdder) AddMany(ctx context.Context, nodes []ipld.Node) error {
// 	for _, node := range nodes {
// 		err := ba.Add(ctx, node)
// 		if err != nil {
// 			return err
// 		}
// 	}
// 	return nil
// }

// IpldNodeToNodeWithMeta converts an ipld.Node to api.NodeWithMeta.
func IpldNodeToNodeWithMeta(n ipld.Node) api.NodeWithMeta {
	size, err := n.Size()
	if err != nil {
		logger.Warn(err)
	}

	return api.NodeWithMeta{
		Cid:     n.Cid(),
		Data:    n.RawData(),
		CumSize: size,
	}
}

// BlockAllocate helps allocating blocks to peers.
func BlockAllocate(ctx context.Context, rpc *rpc.Client, pinOpts api.PinOptions) ([]peer.ID, error) {
	// Find where to allocate this file
	var allocsStr []peer.ID
	err := rpc.CallContext(
		ctx,
		"",
		"Cluster",
		"BlockAllocate",
		api.PinWithOpts(cid.Undef, pinOpts),
		&allocsStr,
	)
	return allocsStr, err
}

// Pin helps sending local RPC pin requests.
func Pin(ctx context.Context, rpc *rpc.Client, pin api.Pin) error {
	if pin.ReplicationFactorMin < 0 {
		pin.Allocations = []peer.ID{}
	}
	logger.Debugf("adder pinning %+v", pin)
	var pinResp api.Pin
	return rpc.CallContext(
		ctx,
		"", // use ourself to pin
		"Cluster",
		"Pin",
		pin,
		&pinResp,
	)
}

// ErrDAGNotFound is returned whenever we try to get a block from the DAGService.
var ErrDAGNotFound = errors.New("dagservice: block not found")

// BaseDAGService partially implements an ipld.DAGService.
// It provides the methods which are not needed by ClusterDAGServices
// (Get*, Remove*) so that they can save adding this code.
type BaseDAGService struct {
}

// Get always returns errNotFound
func (dag BaseDAGService) Get(ctx context.Context, key cid.Cid) (ipld.Node, error) {
	return nil, ErrDAGNotFound
}

// GetMany returns an output channel that always emits an error
func (dag BaseDAGService) GetMany(ctx context.Context, keys []cid.Cid) <-chan *ipld.NodeOption {
	out := make(chan *ipld.NodeOption, 1)
	out <- &ipld.NodeOption{Err: fmt.Errorf("failed to fetch all nodes")}
	close(out)
	return out
}

// Remove is a nop
func (dag BaseDAGService) Remove(ctx context.Context, key cid.Cid) error {
	return nil
}

// RemoveMany is a nop
func (dag BaseDAGService) RemoveMany(ctx context.Context, keys []cid.Cid) error {
	return nil
}
