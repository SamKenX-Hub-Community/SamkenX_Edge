package polybft

import (
	"encoding/hex"
	"errors"
	"math/big"
	"strconv"
	"testing"

	"github.com/umbracle/ethgo/abi"
	"github.com/umbracle/ethgo/jsonrpc"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/umbracle/ethgo"

	"github.com/0xPolygon/polygon-edge/consensus/polybft/bitmap"
	polyCommon "github.com/0xPolygon/polygon-edge/consensus/polybft/common"
	"github.com/0xPolygon/polygon-edge/consensus/polybft/contractsapi"
	bls "github.com/0xPolygon/polygon-edge/consensus/polybft/signer"
	"github.com/0xPolygon/polygon-edge/consensus/polybft/validator"
	"github.com/0xPolygon/polygon-edge/consensus/polybft/wallet"
	"github.com/0xPolygon/polygon-edge/contracts"
	"github.com/0xPolygon/polygon-edge/helper/common"
	"github.com/0xPolygon/polygon-edge/merkle-tree"
	"github.com/0xPolygon/polygon-edge/txrelayer"
	"github.com/0xPolygon/polygon-edge/types"
)

func TestCheckpointManager_SubmitCheckpoint(t *testing.T) {
	t.Parallel()

	const (
		blocksCount = 10
		epochSize   = 2
	)

	var aliases = []string{"A", "B", "C", "D", "E"}

	validators := validator.NewTestValidatorsWithAliases(t, aliases)
	validatorsMetadata := validators.GetPublicIdentities()
	txRelayerMock := newDummyTxRelayer(t)
	txRelayerMock.On("Call", mock.Anything, mock.Anything, mock.Anything).
		Return("2", error(nil)).
		Once()
	txRelayerMock.On("SendTransaction", mock.Anything, mock.Anything).
		Return(&ethgo.Receipt{Status: uint64(types.ReceiptSuccess)}, error(nil)).
		Times(4) // send transactions for checkpoint blocks: 4, 6, 8 (pending checkpoint blocks) and 10 (latest checkpoint block)

	backendMock := new(polybftBackendMock)
	backendMock.On("GetValidators", mock.Anything, mock.Anything).Return(validatorsMetadata)

	var (
		headersMap  = &testHeadersMap{}
		epochNumber = uint64(1)
		dummyMsg    = []byte("checkpoint")
		idx         = uint64(0)
		header      *types.Header
		bitmap      bitmap.Bitmap
		signatures  bls.Signatures
	)

	validators.IterAcct(aliases, func(t *validator.TestValidator) {
		bitmap.Set(idx)
		signatures = append(signatures, t.MustSign(dummyMsg, bls.DomainCheckpointManager))
		idx++
	})

	signature, err := signatures.Aggregate().Marshal()
	require.NoError(t, err)

	for i := uint64(1); i <= blocksCount; i++ {
		if i%epochSize == 1 {
			// epoch-beginning block
			checkpoint := &CheckpointData{
				BlockRound:  0,
				EpochNumber: epochNumber,
				EventRoot:   types.BytesToHash(generateRandomBytes(t)),
			}
			extra := createTestExtraObject(validatorsMetadata, validatorsMetadata, 3, 3, 3)
			extra.Checkpoint = checkpoint
			extra.Committed = &Signature{Bitmap: bitmap, AggregatedSignature: signature}
			header = &types.Header{
				ExtraData: extra.MarshalRLPTo(nil),
			}
			epochNumber++
		} else {
			header = header.Copy()
		}

		header.Number = i
		header.ComputeHash()
		headersMap.addHeader(header)
	}

	// mock blockchain
	blockchainMock := new(blockchainMock)
	blockchainMock.On("GetHeaderByNumber", mock.Anything).Return(headersMap.getHeader)

	validatorAcc := validators.GetValidator("A")
	c := &checkpointManager{
		key:              wallet.NewEcdsaSigner(validatorAcc.Key()),
		rootChainRelayer: txRelayerMock,
		consensusBackend: backendMock,
		blockchain:       blockchainMock,
		logger:           hclog.NewNullLogger(),
	}

	err = c.submitCheckpoint(headersMap.getHeader(blocksCount), false)
	require.NoError(t, err)
	txRelayerMock.AssertExpectations(t)

	// make sure that expected blocks are checkpointed (epoch-ending ones)
	for _, checkpointBlock := range txRelayerMock.checkpointBlocks {
		header := headersMap.getHeader(checkpointBlock)
		require.NotNil(t, header)
		require.True(t, isEndOfPeriod(header.Number, epochSize))
	}
}

func TestCheckpointManager_abiEncodeCheckpointBlock(t *testing.T) {
	t.Parallel()

	const epochSize = uint64(10)

	currentValidators := validator.NewTestValidatorsWithAliases(t, []string{"A", "B", "C", "D"})
	nextValidators := validator.NewTestValidatorsWithAliases(t, []string{"E", "F", "G", "H"})
	header := &types.Header{Number: 50}
	checkpoint := &CheckpointData{
		BlockRound:  1,
		EpochNumber: getEpochNumber(t, header.Number, epochSize),
		EventRoot:   types.BytesToHash(generateRandomBytes(t)),
	}

	proposalHash := generateRandomBytes(t)

	bmp := bitmap.Bitmap{}
	i := uint64(0)

	var signatures bls.Signatures

	currentValidators.IterAcct(nil, func(v *validator.TestValidator) {
		signatures = append(signatures, v.MustSign(proposalHash, bls.DomainCheckpointManager))
		bmp.Set(i)
		i++
	})

	aggSignature, err := signatures.Aggregate().Marshal()
	require.NoError(t, err)

	extra := &Extra{Checkpoint: checkpoint}
	extra.Committed = &Signature{
		AggregatedSignature: aggSignature,
		Bitmap:              bmp,
	}
	header.ExtraData = extra.MarshalRLPTo(nil)
	header.ComputeHash()

	backendMock := new(polybftBackendMock)
	backendMock.On("GetValidators", mock.Anything, mock.Anything).Return(currentValidators.GetPublicIdentities())

	c := &checkpointManager{
		blockchain:       &blockchainMock{},
		consensusBackend: backendMock,
		logger:           hclog.NewNullLogger(),
	}
	checkpointDataEncoded, err := c.abiEncodeCheckpointBlock(header.Number, header.Hash, extra, nextValidators.GetPublicIdentities())
	require.NoError(t, err)

	submit := &contractsapi.SubmitCheckpointManagerFn{}
	require.NoError(t, submit.DecodeAbi(checkpointDataEncoded))

	require.Equal(t, new(big.Int).SetUint64(checkpoint.EpochNumber), submit.Checkpoint.Epoch)
	require.Equal(t, new(big.Int).SetUint64(header.Number), submit.Checkpoint.BlockNumber)
	require.Equal(t, checkpoint.EventRoot, submit.Checkpoint.EventRoot)
	require.Equal(t, new(big.Int).SetUint64(checkpoint.BlockRound), submit.CheckpointMetadata.BlockRound)
}

func TestCheckpointManager_getCurrentCheckpointID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		checkpointID string
		returnError  error
		errSubstring string
	}{
		{
			name:         "Happy path",
			checkpointID: "16",
			returnError:  error(nil),
			errSubstring: "",
		},
		{
			name:         "Rootchain call returns an error",
			checkpointID: "",
			returnError:  errors.New("internal error"),
			errSubstring: "failed to invoke currentCheckpointId function on the rootchain",
		},
		{
			name:         "Failed to parse return value from rootchain",
			checkpointID: "Hello World!",
			returnError:  error(nil),
			errSubstring: "failed to convert current checkpoint id",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			txRelayerMock := newDummyTxRelayer(t)
			txRelayerMock.On("Call", mock.Anything, mock.Anything, mock.Anything).
				Return(c.checkpointID, c.returnError).
				Once()
			acc, err := wallet.GenerateAccount()
			require.NoError(t, err)

			checkpointMgr := &checkpointManager{
				rootChainRelayer: txRelayerMock,
				key:              acc.Ecdsa,
				logger:           hclog.NewNullLogger(),
			}
			actualCheckpointID, err := checkpointMgr.getLatestCheckpointBlock()
			if c.errSubstring == "" {
				expectedCheckpointID, err := strconv.ParseUint(c.checkpointID, 0, 64)
				require.NoError(t, err)
				require.Equal(t, expectedCheckpointID, actualCheckpointID)
			} else {
				require.ErrorContains(t, err, c.errSubstring)
			}

			txRelayerMock.AssertExpectations(t)
		})
	}
}

func TestCheckpointManager_IsCheckpointBlock(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name               string
		blockNumber        uint64
		checkpointsOffset  uint64
		isEpochEndingBlock bool
		isCheckpointBlock  bool
	}{
		{
			name:               "Not checkpoint block",
			blockNumber:        3,
			checkpointsOffset:  6,
			isEpochEndingBlock: false,
			isCheckpointBlock:  false,
		},
		{
			name:               "Checkpoint block",
			blockNumber:        6,
			checkpointsOffset:  6,
			isEpochEndingBlock: false,
			isCheckpointBlock:  true,
		},
		{
			name:               "Epoch ending block - Fixed epoch size met",
			blockNumber:        10,
			checkpointsOffset:  5,
			isEpochEndingBlock: true,
			isCheckpointBlock:  true,
		},
		{
			name:               "Epoch ending block - Epoch ended before fix size was met",
			blockNumber:        9,
			checkpointsOffset:  5,
			isEpochEndingBlock: true,
			isCheckpointBlock:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			checkpointMgr := newCheckpointManager(wallet.NewEcdsaSigner(createTestKey(t)),
				types.ZeroAddress, nil, nil, nil, hclog.NewNullLogger(), nil)
			require.Equal(t, c.isCheckpointBlock,
				checkpointMgr.isCheckpointBlock(c.blockNumber, c.checkpointsOffset, c.isEpochEndingBlock))
		})
	}
}

func TestCheckpointManager_PostBlock(t *testing.T) {
	const (
		numOfReceipts = 5
		block         = 5
		epoch         = 1
	)

	state := newTestState(t)

	createReceipts := func(startID, endID uint64) []*types.Receipt {
		receipts := make([]*types.Receipt, endID-startID)
		for i := startID; i < endID; i++ {
			receipts[i-startID] = &types.Receipt{Logs: []*types.Log{
				createTestLogForExitEvent(t, i),
			}}
			receipts[i-startID].SetStatus(types.ReceiptSuccess)
		}

		return receipts
	}

	extra := &Extra{
		Checkpoint: &CheckpointData{
			EpochNumber: epoch,
		},
	}

	req := &polyCommon.PostBlockRequest{
		FullBlock: &types.FullBlock{
			Block: &types.Block{
				Header: &types.Header{Number: block},
			},
		},
		Epoch:               epoch,
		CurrentClientConfig: createTestPolybftConfig(),
	}

	req.FullBlock.Block.Header.ExtraData = extra.MarshalRLPTo(nil)

	blockchain := new(blockchainMock)
	checkpointManager := newCheckpointManager(wallet.NewEcdsaSigner(createTestKey(t)), types.ZeroAddress,
		nil, blockchain, nil, hclog.NewNullLogger(), state)

	t.Run("PostBlock - not epoch ending block", func(t *testing.T) {
		require.NoError(t, state.ExitEventStore.updateLastSaved(block-1)) // we got everything till the current block
		req.IsEpochEndingBlock = false
		req.FullBlock.Receipts = createReceipts(0, 5)
		require.NoError(t, checkpointManager.PostBlock(req))

		exitEvents, err := state.ExitEventStore.getExitEvents(epoch, func(exitEvent *ExitEvent) bool {
			return exitEvent.BlockNumber == block
		})

		require.NoError(t, err)
		require.Len(t, exitEvents, 5)
		require.Equal(t, uint64(epoch), exitEvents[0].EpochNumber)
	})

	t.Run("PostBlock - epoch ending block (exit events are saved to the next epoch)", func(t *testing.T) {
		require.NoError(t, state.ExitEventStore.updateLastSaved(block)) // we got everything till the current block
		req.IsEpochEndingBlock = true
		req.FullBlock.Receipts = createReceipts(5, 10)
		extra.Validators = &validator.ValidatorSetDelta{}
		req.FullBlock.Block.Header.ExtraData = extra.MarshalRLPTo(nil)
		req.FullBlock.Block.Header.Number = block + 1

		require.NoError(t, checkpointManager.PostBlock(req))

		exitEvents, err := state.ExitEventStore.getExitEvents(epoch+1, func(exitEvent *ExitEvent) bool {
			return exitEvent.BlockNumber == block+2 // they should be saved in the next epoch and its first block
		})

		require.NoError(t, err)
		require.Len(t, exitEvents, 5)
		require.Equal(t, uint64(block+2), exitEvents[0].BlockNumber)
		require.Equal(t, uint64(epoch+1), exitEvents[0].EpochNumber)
	})

	t.Run("PostBlock - there are missing events", func(t *testing.T) {
		require.NoError(t, state.ExitEventStore.updateLastSaved(block)) // we are missing one block

		missedReceipts := createReceipts(10, 13)
		newReceipts := createReceipts(13, 15)

		extra := &Extra{
			Checkpoint: &CheckpointData{
				EpochNumber: epoch + 1,
			},
		}

		blockchain.On("GetHeaderByNumber", uint64(block+1)).Return(&types.Header{
			Number:    block + 1,
			ExtraData: extra.MarshalRLPTo(nil),
			Hash:      types.BytesToHash([]byte{0, 1, 2, 3}),
		}, true)
		blockchain.On("GetReceiptsByHash", types.BytesToHash([]byte{0, 1, 2, 3})).Return([]*types.Receipt{}, nil)
		blockchain.On("GetHeaderByNumber", uint64(block+2)).Return(&types.Header{
			Number:    block + 2,
			ExtraData: extra.MarshalRLPTo(nil),
			Hash:      types.BytesToHash([]byte{4, 5, 6, 7}),
		}, true)
		blockchain.On("GetReceiptsByHash", types.BytesToHash([]byte{4, 5, 6, 7})).Return(missedReceipts, nil)

		req.IsEpochEndingBlock = false
		req.FullBlock.Block.Header.Number = block + 3                  // new block
		req.FullBlock.Block.Header.ExtraData = extra.MarshalRLPTo(nil) // same epoch
		req.FullBlock.Receipts = newReceipts
		require.NoError(t, checkpointManager.PostBlock(req))

		exitEvents, err := state.ExitEventStore.getExitEvents(epoch+1, func(exitEvent *ExitEvent) bool {
			return exitEvent.BlockNumber == block+2
		})

		require.NoError(t, err)
		// receipts from missed block + events from previous test case that were saved in the next epoch
		// since they were in epoch ending block
		require.Len(t, exitEvents, len(missedReceipts)+5)
		require.Equal(t, extra.Checkpoint.EpochNumber, exitEvents[0].EpochNumber)

		exitEvents, err = state.ExitEventStore.getExitEvents(epoch+1, func(exitEvent *ExitEvent) bool {
			return exitEvent.BlockNumber == block+3
		})

		require.NoError(t, err)
		require.Len(t, exitEvents, len(newReceipts))
		require.Equal(t, extra.Checkpoint.EpochNumber, exitEvents[0].EpochNumber)
	})
}

func TestCheckpointManager_BuildEventRoot(t *testing.T) {
	t.Parallel()

	const (
		numOfBlocks         = 10
		numOfEventsPerBlock = 2
	)

	state := newTestState(t)
	checkpointManager := &checkpointManager{state: state}

	encodedEvents := insertTestExitEvents(t, state, 1, numOfBlocks, numOfEventsPerBlock)

	t.Run("Get exit event root hash", func(t *testing.T) {
		t.Parallel()

		tree, err := createExitTree(encodedEvents)
		require.NoError(t, err)

		hash, err := checkpointManager.BuildEventRoot(1)
		require.NoError(t, err)
		require.Equal(t, tree.Hash(), hash)
	})

	t.Run("Get exit event root hash - no events", func(t *testing.T) {
		t.Parallel()

		hash, err := checkpointManager.BuildEventRoot(2)
		require.NoError(t, err)
		require.Equal(t, types.Hash{}, hash)
	})
}

func TestCheckpointManager_GenerateExitProof(t *testing.T) {
	t.Parallel()

	const (
		numOfBlocks           = 10
		numOfEventsPerBlock   = 2
		correctBlockToGetExit = 1
		futureBlockToGetExit  = 2
	)

	state := newTestState(t)

	// setup mocks for valid case
	foundCheckpointReturn, err := contractsapi.GetCheckpointBlockABIResponse.Encode(map[string]interface{}{
		"isFound":         true,
		"checkpointBlock": 1,
	})
	require.NoError(t, err)

	getCheckpointBlockFn := &contractsapi.GetCheckpointBlockCheckpointManagerFn{
		BlockNumber: new(big.Int).SetUint64(correctBlockToGetExit),
	}

	input, err := getCheckpointBlockFn.EncodeAbi()
	require.NoError(t, err)

	dummyTxRelayer := newDummyTxRelayer(t)
	dummyTxRelayer.On("Call", ethgo.ZeroAddress, ethgo.ZeroAddress, input).
		Return(hex.EncodeToString(foundCheckpointReturn), error(nil))

	// create checkpoint manager and insert exit events
	checkpointMgr := newCheckpointManager(wallet.NewEcdsaSigner(
		createTestKey(t)),
		types.ZeroAddress,
		dummyTxRelayer,
		nil,
		nil,
		hclog.NewNullLogger(),
		state)

	exitEvents := insertTestExitEvents(t, state, 1, numOfBlocks, numOfEventsPerBlock)
	encodedEvents := encodeExitEvents(t, exitEvents)
	checkpointEvents := encodedEvents[:numOfEventsPerBlock]

	// manually create merkle tree for a desired checkpoint to verify the generated proof
	tree, err := merkle.NewMerkleTree(checkpointEvents)
	require.NoError(t, err)

	proof, err := checkpointMgr.GenerateExitProof(correctBlockToGetExit)
	require.NoError(t, err)
	require.NotNil(t, proof)

	t.Run("Generate and validate exit proof", func(t *testing.T) {
		t.Parallel()
		// verify generated proof on desired tree
		require.NoError(t, merkle.VerifyProof(correctBlockToGetExit, encodedEvents[1], proof.Data, tree.Hash()))
	})

	t.Run("Generate and validate exit proof - invalid proof", func(t *testing.T) {
		t.Parallel()

		// copy and make proof invalid
		invalidProof := make([]types.Hash, len(proof.Data))
		copy(invalidProof, proof.Data)
		invalidProof[0][0]++

		// verify generated proof on desired tree
		require.ErrorContains(t, merkle.VerifyProof(correctBlockToGetExit,
			encodedEvents[1], invalidProof, tree.Hash()), "not a member of merkle tree")
	})

	t.Run("Generate exit proof - no event", func(t *testing.T) {
		t.Parallel()

		_, err := checkpointMgr.GenerateExitProof(21)
		require.ErrorContains(t, err, "could not find any exit event that has an id")
	})

	t.Run("Generate exit proof - future lookup where checkpoint not yet submitted", func(t *testing.T) {
		t.Parallel()

		// setup mocks for invalid case
		notFoundCheckpointReturn, err := contractsapi.GetCheckpointBlockABIResponse.Encode(map[string]interface{}{
			"isFound":         false,
			"checkpointBlock": 0,
		})
		require.NoError(t, err)

		getCheckpointBlockFn.BlockNumber = new(big.Int).SetUint64(futureBlockToGetExit)
		inputTwo, err := getCheckpointBlockFn.EncodeAbi()
		require.NoError(t, err)

		dummyTxRelayer.On("Call", ethgo.ZeroAddress, ethgo.ZeroAddress, inputTwo).
			Return(hex.EncodeToString(notFoundCheckpointReturn), error(nil))

		_, err = checkpointMgr.GenerateExitProof(futureBlockToGetExit)
		require.ErrorContains(t, err, "checkpoint block not found for exit ID")
	})
}

func TestCheckpointManager_GenerateSlashExitProofs(t *testing.T) {
	t.Parallel()

	const (
		numOfBlocks           = 10
		numOfEventsPerBlock   = 2
		correctBlockToGetExit = 1
		futureBlockToGetExit  = 2
	)

	state := newTestState(t)

	// setup mocks for valid case
	foundCheckpointReturn, err := contractsapi.GetCheckpointBlockABIResponse.Encode(map[string]interface{}{
		"isFound":         true,
		"checkpointBlock": 1,
	})
	require.NoError(t, err)

	dummyTxRelayer := newDummyTxRelayer(t)
	dummyTxRelayer.On("Call", mock.Anything, mock.Anything, mock.Anything).
		Return(hex.EncodeToString(foundCheckpointReturn), error(nil))

	// create checkpoint manager and insert exit events
	checkpointMgr := newCheckpointManager(wallet.NewEcdsaSigner(
		createTestKey(t)),
		types.ZeroAddress,
		dummyTxRelayer,
		nil,
		nil,
		hclog.NewNullLogger(),
		state)

	exitEvents := insertTestExitEvents(t, state, 1, numOfBlocks, numOfEventsPerBlock)
	encodedEvents := encodeExitEvents(t, exitEvents)
	checkpointEvents := encodedEvents[:numOfEventsPerBlock]

	// manually create merkle tree for a desired checkpoint to verify the generated proof
	tree, err := merkle.NewMerkleTree(checkpointEvents)
	require.NoError(t, err)

	proofs, err := checkpointMgr.GenerateSlashExitProofs()
	require.NoError(t, err)
	require.Len(t, proofs, len(checkpointEvents))

	t.Run("Generate and validate exit proof", func(t *testing.T) {
		t.Parallel()
		// verify generated proof on desired tree
		require.NoError(t, merkle.VerifyProof(correctBlockToGetExit, encodedEvents[1], proofs[1].Data, tree.Hash()))
	})

	t.Run("Generate and validate exit proof - invalid proof", func(t *testing.T) {
		t.Parallel()

		proof := proofs[0]
		// copy and make proof invalid
		invalidProof := make([]types.Hash, len(proof.Data))
		copy(invalidProof, proof.Data)
		invalidProof[0][0]++

		// verify generated proof on desired tree
		require.ErrorContains(t, merkle.VerifyProof(correctBlockToGetExit,
			encodedEvents[1], invalidProof, tree.Hash()), "not a member of merkle tree")
	})

	t.Run("Generate exit proof - no event", func(t *testing.T) {
		t.Parallel()

		_, err := checkpointMgr.GenerateExitProof(21)
		require.ErrorContains(t, err, "could not find any exit event that has an id")
	})
}

var _ txrelayer.TxRelayer = (*dummyTxRelayer)(nil)

type dummyTxRelayer struct {
	mock.Mock

	test             *testing.T
	checkpointBlocks []uint64
}

func newDummyTxRelayer(t *testing.T) *dummyTxRelayer {
	t.Helper()

	return &dummyTxRelayer{test: t}
}

func (d *dummyTxRelayer) Call(from ethgo.Address, to ethgo.Address, input []byte) (string, error) {
	args := d.Called(from, to, input)

	return args.String(0), args.Error(1)
}

func (d *dummyTxRelayer) SendTransaction(transaction *ethgo.Transaction, key ethgo.Key) (*ethgo.Receipt, error) {
	blockNumber := getBlockNumberCheckpointSubmitInput(d.test, transaction.Input)
	d.checkpointBlocks = append(d.checkpointBlocks, blockNumber)
	args := d.Called(transaction, key)

	return args.Get(0).(*ethgo.Receipt), args.Error(1) //nolint:forcetypeassert
}

// SendTransactionLocal sends non-signed transaction (this is only for testing purposes)
func (d *dummyTxRelayer) SendTransactionLocal(txn *ethgo.Transaction) (*ethgo.Receipt, error) {
	args := d.Called(txn)

	return args.Get(0).(*ethgo.Receipt), args.Error(1) //nolint:forcetypeassert
}

func (d *dummyTxRelayer) Client() *jsonrpc.Client {
	return nil
}

func getBlockNumberCheckpointSubmitInput(t *testing.T, input []byte) uint64 {
	t.Helper()

	submit := &contractsapi.SubmitCheckpointManagerFn{}
	require.NoError(t, submit.DecodeAbi(input))

	return submit.Checkpoint.BlockNumber.Uint64()
}

func createTestLogForExitEvent(t *testing.T, exitEventID uint64) *types.Log {
	t.Helper()

	var exitEvent contractsapi.L2StateSyncedEvent

	topics := make([]types.Hash, 4)
	topics[0] = types.Hash(exitEvent.Sig())                                // function signature
	topics[1] = types.BytesToHash(common.EncodeUint64ToBytes(exitEventID)) // ID
	topics[2] = types.BytesToHash(types.StringToAddress("0x1111").Bytes()) // Sender
	topics[3] = types.BytesToHash(types.StringToAddress("0x2222").Bytes()) // Receiver
	sigType := abi.MustNewType("tuple(bytes signature)")                   // Data
	encodedData, err := sigType.Encode(map[string]interface{}{"signature": slashSignature})
	require.NoError(t, err)

	return &types.Log{
		Address: contracts.L2StateSenderContract,
		Topics:  topics,
		Data:    encodedData,
	}
}
