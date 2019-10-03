package block

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ElrondNetwork/elrond-go/core"
	"github.com/ElrondNetwork/elrond-go/core/serviceContainer"
	"github.com/ElrondNetwork/elrond-go/data"
	"github.com/ElrondNetwork/elrond-go/data/block"
	"github.com/ElrondNetwork/elrond-go/dataRetriever"
	"github.com/ElrondNetwork/elrond-go/dataRetriever/dataPool"
	"github.com/ElrondNetwork/elrond-go/process"
	"github.com/ElrondNetwork/elrond-go/process/throttle"
	"github.com/ElrondNetwork/elrond-go/sharding"
	"github.com/ElrondNetwork/elrond-go/statusHandler"
)

const maxCleanTime = time.Second

type nonceAndHashInfo struct {
	hash  []byte
	nonce uint64
}

type hdrInfo struct {
	usedInBlock bool
	hdr         data.HeaderHandler
}

type hdrForBlock struct {
	missingHdrs      uint32
	missingFinalHdrs uint32
	mutHdrsForBlock  sync.RWMutex
	hdrHashAndInfo   map[string]*hdrInfo
}

// shardProcessor implements shardProcessor interface and actually it tries to execute block
type shardProcessor struct {
	*baseProcessor
	dataPool          dataRetriever.PoolsHolder
	metaBlockFinality int

	chRcvAllMetaHdrs        chan bool
	hdrsForCurrBlock        hdrForBlock
	currHighestMetaHdrNonce uint64

	processedMiniBlocks    map[string]map[string]struct{}
	mutProcessedMiniBlocks sync.RWMutex

	core          serviceContainer.Core
	txCoordinator process.TransactionCoordinator
	txCounter     *transactionCounter

	txsPoolsCleaner process.PoolsCleaner
}

// NewShardProcessor creates a new shardProcessor object
func NewShardProcessor(arguments ArgShardProcessor) (*shardProcessor, error) {

	err := checkProcessorNilParameters(
		arguments.Accounts,
		arguments.ForkDetector,
		arguments.Hasher,
		arguments.Marshalizer,
		arguments.Store,
		arguments.ShardCoordinator,
		arguments.NodesCoordinator,
		arguments.SpecialAddressHandler,
		arguments.Uint64Converter)
	if err != nil {
		return nil, err
	}

	if arguments.DataPool == nil || arguments.DataPool.IsInterfaceNil() {
		return nil, process.ErrNilDataPoolHolder
	}
	if arguments.RequestHandler == nil || arguments.RequestHandler.IsInterfaceNil() {
		return nil, process.ErrNilRequestHandler
	}
	if arguments.TxCoordinator == nil || arguments.TxCoordinator.IsInterfaceNil() {
		return nil, process.ErrNilTransactionCoordinator
	}

	blockSizeThrottler, err := throttle.NewBlockSizeThrottle()
	if err != nil {
		return nil, err
	}

	base := &baseProcessor{
		accounts:                      arguments.Accounts,
		blockSizeThrottler:            blockSizeThrottler,
		forkDetector:                  arguments.ForkDetector,
		hasher:                        arguments.Hasher,
		marshalizer:                   arguments.Marshalizer,
		store:                         arguments.Store,
		shardCoordinator:              arguments.ShardCoordinator,
		nodesCoordinator:              arguments.NodesCoordinator,
		specialAddressHandler:         arguments.SpecialAddressHandler,
		uint64Converter:               arguments.Uint64Converter,
		onRequestHeaderHandlerByNonce: arguments.RequestHandler.RequestHeaderByNonce,
		appStatusHandler:              statusHandler.NewNilStatusHandler(),
	}
	err = base.setLastNotarizedHeadersSlice(arguments.StartHeaders)
	if err != nil {
		return nil, err
	}

	if arguments.TxsPoolsCleaner == nil || arguments.TxsPoolsCleaner.IsInterfaceNil() {
		return nil, process.ErrNilTxsPoolsCleaner
	}

	sp := shardProcessor{
		core:            arguments.Core,
		baseProcessor:   base,
		dataPool:        arguments.DataPool,
		txCoordinator:   arguments.TxCoordinator,
		txCounter:       NewTransactionCounter(),
		txsPoolsCleaner: arguments.TxsPoolsCleaner,
	}
	sp.chRcvAllMetaHdrs = make(chan bool)

	transactionPool := sp.dataPool.Transactions()
	if transactionPool == nil {
		return nil, process.ErrNilTransactionPool
	}

	sp.hdrsForCurrBlock.hdrHashAndInfo = make(map[string]*hdrInfo)
	sp.processedMiniBlocks = make(map[string]map[string]struct{})

	metaBlockPool := sp.dataPool.MetaBlocks()
	if metaBlockPool == nil {
		return nil, process.ErrNilMetaBlockPool
	}
	metaBlockPool.RegisterHandler(sp.receivedMetaBlock)
	sp.onRequestHeaderHandler = arguments.RequestHandler.RequestHeader

	sp.metaBlockFinality = process.MetaBlockFinality

	return &sp, nil
}

// ProcessBlock processes a block. It returns nil if all ok or the specific error
func (sp *shardProcessor) ProcessBlock(
	chainHandler data.ChainHandler,
	headerHandler data.HeaderHandler,
	bodyHandler data.BodyHandler,
	haveTime func() time.Duration,
) error {

	if haveTime == nil {
		return process.ErrNilHaveTimeHandler
	}

	err := sp.checkBlockValidity(chainHandler, headerHandler, bodyHandler)
	if err != nil {
		if err == process.ErrBlockHashDoesNotMatch {
			log.Info(fmt.Sprintf("requested missing shard header with hash %s for shard %d\n",
				core.ToB64(headerHandler.GetPrevHash()),
				headerHandler.GetShardID()))

			go sp.onRequestHeaderHandler(headerHandler.GetShardID(), headerHandler.GetPrevHash())
		}

		return err
	}

	log.Debug(fmt.Sprintf("started processing block with round %d and nonce %d\n",
		headerHandler.GetRound(),
		headerHandler.GetNonce()))

	header, ok := headerHandler.(*block.Header)
	if !ok {
		return process.ErrWrongTypeAssertion
	}

	body, ok := bodyHandler.(block.Body)
	if !ok {
		return process.ErrWrongTypeAssertion
	}

	go getMetricsFromBlockBody(body, sp.marshalizer, sp.appStatusHandler)

	err = sp.checkHeaderBodyCorrelation(header, body)
	if err != nil {
		return err
	}

	numTxWithDst := sp.txCounter.getNumTxsFromPool(header.ShardId, sp.dataPool, sp.shardCoordinator.NumberOfShards())
	totalTxs := sp.txCounter.totalTxs
	go getMetricsFromHeader(header, uint64(numTxWithDst), totalTxs, sp.marshalizer, sp.appStatusHandler)

	log.Info(fmt.Sprintf("Total txs in pool: %d\n", numTxWithDst))

	err = sp.specialAddressHandler.SetShardConsensusData(
		headerHandler.GetPrevRandSeed(),
		headerHandler.GetRound(),
		headerHandler.GetEpoch(),
		headerHandler.GetShardID(),
	)
	if err != nil {
		return err
	}

	sp.CreateBlockStarted()
	sp.txCoordinator.RequestBlockTransactions(body)
	requestedMetaHdrs, requestedFinalMetaHdrs := sp.requestMetaHeaders(header)

	if haveTime() < 0 {
		return process.ErrTimeIsOut
	}

	err = sp.txCoordinator.IsDataPreparedForProcessing(haveTime)
	if err != nil {
		return err
	}

	if requestedMetaHdrs > 0 || requestedFinalMetaHdrs > 0 {
		log.Info(fmt.Sprintf("requested %d missing meta headers and %d final meta headers\n", requestedMetaHdrs, requestedFinalMetaHdrs))
		err = sp.waitForMetaHdrHashes(haveTime())

		sp.hdrsForCurrBlock.mutHdrsForBlock.Lock()
		missingHdrs := sp.hdrsForCurrBlock.missingHdrs
		sp.hdrsForCurrBlock.missingHdrs = 0
		sp.hdrsForCurrBlock.missingFinalHdrs = 0
		sp.hdrsForCurrBlock.mutHdrsForBlock.Unlock()

		if requestedMetaHdrs > 0 {
			log.Info(fmt.Sprintf("received %d missing meta headers\n", requestedMetaHdrs-missingHdrs))
		}

		if err != nil {
			return err
		}
	}

	if sp.accounts.JournalLen() != 0 {
		return process.ErrAccountStateDirty
	}

	defer func() {
		go sp.checkAndRequestIfMetaHeadersMissing(header.Round)
	}()

	err = sp.checkMetaHeadersValidityAndFinality()
	if err != nil {
		return err
	}

	err = sp.verifyCrossShardMiniBlockDstMe(header)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			sp.RevertAccountState()
		}
	}()

	processedMetaHdrs, err := sp.getProcessedMetaBlocksFromMiniBlocks(body)
	if err != nil {
		return err
	}

	err = sp.setMetaConsensusData(processedMetaHdrs)
	if err != nil {
		return err
	}

	err = sp.txCoordinator.ProcessBlockTransaction(body, header.Round, haveTime)
	if err != nil {
		return err
	}

	if !sp.verifyStateRoot(header.GetRootHash()) {
		err = process.ErrRootStateDoesNotMatch
		return err
	}

	err = sp.txCoordinator.VerifyCreatedBlockTransactions(body)
	if err != nil {
		return err
	}

	return nil
}

func (sp *shardProcessor) setMetaConsensusData(finalizedMetaBlocks []data.HeaderHandler) error {
	sp.specialAddressHandler.ClearMetaConsensusData()

	// for every finalized metablock header, reward the metachain consensus group members with accounts in shard
	for _, metaBlock := range finalizedMetaBlocks {
		round := metaBlock.GetRound()
		epoch := metaBlock.GetEpoch()
		err := sp.specialAddressHandler.SetMetaConsensusData(metaBlock.GetPrevRandSeed(), round, epoch)
		if err != nil {
			return err
		}
	}

	return nil
}

// SetConsensusData - sets the reward data for the current consensus group
func (sp *shardProcessor) SetConsensusData(randomness []byte, round uint64, epoch uint32, shardId uint32) {
	err := sp.specialAddressHandler.SetShardConsensusData(randomness, round, epoch, shardId)
	if err != nil {
		log.Error(err.Error())
	}
}

// checkMetaHeadersValidity - checks if listed metaheaders are valid as construction
func (sp *shardProcessor) checkMetaHeadersValidityAndFinality() error {
	tmpNotedHdr, err := sp.getLastNotarizedHdr(sharding.MetachainShardId)
	if err != nil {
		return err
	}

	usedMetaHdrs, err := sp.sortHdrsForCurrentBlock(true)
	if err != nil {
		return err
	}

	if len(usedMetaHdrs) == 0 {
		return nil
	}

	for _, metaHdr := range usedMetaHdrs {
		err = sp.isHdrConstructionValid(metaHdr, tmpNotedHdr)
		if err != nil {
			return err
		}

		tmpNotedHdr = metaHdr
	}

	err = sp.checkMetaHdrFinality(tmpNotedHdr)
	if err != nil {
		return err
	}

	return nil
}

// check if shard headers are final by checking if newer headers were constructed upon them
func (sp *shardProcessor) checkMetaHdrFinality(header data.HeaderHandler) error {
	if header == nil || header.IsInterfaceNil() {
		return process.ErrNilBlockHeader
	}

	finalMetaHdrs, err := sp.sortHdrsForCurrentBlock(false)
	if err != nil {
		return err
	}

	lastVerifiedHdr := header
	// verify if there are "K" block after current to make this one final
	nextBlocksVerified := 0
	for _, metaHdr := range finalMetaHdrs {
		if nextBlocksVerified >= sp.metaBlockFinality {
			break
		}

		// found a header with the next nonce
		if metaHdr.Nonce == lastVerifiedHdr.GetNonce()+1 {
			err := sp.isHdrConstructionValid(metaHdr, lastVerifiedHdr)
			if err != nil {
				log.Debug(err.Error())
				continue
			}

			lastVerifiedHdr = metaHdr
			nextBlocksVerified += 1
		}
	}

	if nextBlocksVerified < sp.metaBlockFinality {
		go sp.onRequestHeaderHandlerByNonce(lastVerifiedHdr.GetShardID(), lastVerifiedHdr.GetNonce()+1)
		return process.ErrHeaderNotFinal
	}

	return nil
}

// check if header has the same miniblocks as presented in body
func (sp *shardProcessor) checkHeaderBodyCorrelation(hdr *block.Header, body block.Body) error {
	mbHashesFromHdr := make(map[string]*block.MiniBlockHeader)
	for i := 0; i < len(hdr.MiniBlockHeaders); i++ {
		mbHashesFromHdr[string(hdr.MiniBlockHeaders[i].Hash)] = &hdr.MiniBlockHeaders[i]
	}

	if len(hdr.MiniBlockHeaders) != len(body) {
		return process.ErrHeaderBodyMismatch
	}

	for i := 0; i < len(body); i++ {
		miniBlock := body[i]

		mbBytes, err := sp.marshalizer.Marshal(miniBlock)
		if err != nil {
			return err
		}
		mbHash := sp.hasher.Compute(string(mbBytes))

		mbHdr, ok := mbHashesFromHdr[string(mbHash)]
		if !ok {
			return process.ErrHeaderBodyMismatch
		}

		if mbHdr.TxCount != uint32(len(miniBlock.TxHashes)) {
			return process.ErrHeaderBodyMismatch
		}

		if mbHdr.ReceiverShardID != miniBlock.ReceiverShardID {
			return process.ErrHeaderBodyMismatch
		}

		if mbHdr.SenderShardID != miniBlock.SenderShardID {
			return process.ErrHeaderBodyMismatch
		}
	}

	return nil
}

func (sp *shardProcessor) checkAndRequestIfMetaHeadersMissing(round uint64) {
	orderedMetaBlocks, err := sp.getOrderedMetaBlocks(round)
	if err != nil {
		log.Debug(err.Error())
		return
	}

	sortedHdrs := make([]data.HeaderHandler, 0)
	for i := 0; i < len(orderedMetaBlocks); i++ {
		hdr, ok := orderedMetaBlocks[i].hdr.(*block.MetaBlock)
		if !ok {
			continue
		}
		sortedHdrs = append(sortedHdrs, hdr)
	}

	err = sp.requestHeadersIfMissing(sortedHdrs, sharding.MetachainShardId, round)
	if err != nil {
		log.Info(err.Error())
	}

	return
}

func (sp *shardProcessor) indexBlockIfNeeded(
	body data.BodyHandler,
	header data.HeaderHandler) {
	if sp.core == nil || sp.core.Indexer() == nil {
		return
	}

	txPool := sp.txCoordinator.GetAllCurrentUsedTxs(block.TxBlock)
	scPool := sp.txCoordinator.GetAllCurrentUsedTxs(block.SmartContractResultBlock)
	rewardPool := sp.txCoordinator.GetAllCurrentUsedTxs(block.RewardsBlock)

	for hash, tx := range scPool {
		txPool[hash] = tx
	}
	for hash, tx := range rewardPool {
		txPool[hash] = tx
	}

	go sp.core.Indexer().SaveBlock(body, header, txPool)
}

// RestoreBlockIntoPools restores the TxBlock and MetaBlock into associated pools
func (sp *shardProcessor) RestoreBlockIntoPools(headerHandler data.HeaderHandler, bodyHandler data.BodyHandler) error {
	sp.removeLastNotarized()

	if headerHandler == nil || headerHandler.IsInterfaceNil() {
		return process.ErrNilBlockHeader
	}
	if bodyHandler == nil || bodyHandler.IsInterfaceNil() {
		return process.ErrNilTxBlockBody
	}

	body, ok := bodyHandler.(block.Body)
	if !ok {
		return process.ErrWrongTypeAssertion
	}

	header, ok := headerHandler.(*block.Header)
	if !ok {
		return process.ErrWrongTypeAssertion
	}

	restoredTxNr, err := sp.txCoordinator.RestoreBlockDataFromStorage(body)
	go sp.txCounter.subtractRestoredTxs(restoredTxNr)
	if err != nil {
		return err
	}

	miniBlockHashes := header.MapMiniBlockHashesToShards()
	err = sp.restoreMetaBlockIntoPool(miniBlockHashes, header.MetaBlockHashes)
	if err != nil {
		return err
	}

	return nil
}

func (sp *shardProcessor) restoreMetaBlockIntoPool(miniBlockHashes map[string]uint32, metaBlockHashes [][]byte) error {
	metaBlockPool := sp.dataPool.MetaBlocks()
	if metaBlockPool == nil {
		return process.ErrNilMetaBlockPool
	}

	metaHeaderNoncesPool := sp.dataPool.HeadersNonces()
	if metaHeaderNoncesPool == nil {
		return process.ErrNilMetaHeadersNoncesDataPool
	}

	for _, metaBlockHash := range metaBlockHashes {
		buff, err := sp.store.Get(dataRetriever.MetaBlockUnit, metaBlockHash)
		if err != nil {
			continue
		}

		metaBlock := block.MetaBlock{}
		err = sp.marshalizer.Unmarshal(&metaBlock, buff)
		if err != nil {
			log.Error(err.Error())
			continue
		}

		processedMiniBlocks := metaBlock.GetMiniBlockHeadersWithDst(sp.shardCoordinator.SelfId())
		for mbHash := range processedMiniBlocks {
			sp.addProcessedMiniBlock(metaBlockHash, []byte(mbHash))
		}

		metaBlockPool.Put(metaBlockHash, &metaBlock)
		syncMap := &dataPool.ShardIdHashSyncMap{}
		syncMap.Store(metaBlock.GetShardID(), metaBlockHash)
		metaHeaderNoncesPool.Merge(metaBlock.Nonce, syncMap)

		err = sp.store.GetStorer(dataRetriever.MetaBlockUnit).Remove(metaBlockHash)
		if err != nil {
			log.Error(err.Error())
		}

		nonceToByteSlice := sp.uint64Converter.ToByteSlice(metaBlock.Nonce)
		err = sp.store.GetStorer(dataRetriever.MetaHdrNonceHashDataUnit).Remove(nonceToByteSlice)
		if err != nil {
			log.Error(err.Error())
		}
	}

	for miniBlockHash := range miniBlockHashes {
		sp.removeProcessedMiniBlock([]byte(miniBlockHash))
	}

	return nil
}

// CreateBlockBody creates a a list of miniblocks by filling them with transactions out of the transactions pools
// as long as the transactions limit for the block has not been reached and there is still time to add transactions
func (sp *shardProcessor) CreateBlockBody(round uint64, haveTime func() bool) (data.BodyHandler, error) {
	log.Debug(fmt.Sprintf("started creating block body in round %d\n", round))
	sp.CreateBlockStarted()
	sp.blockSizeThrottler.ComputeMaxItems()

	miniBlocks, err := sp.createMiniBlocks(sp.shardCoordinator.NumberOfShards(), sp.blockSizeThrottler.MaxItemsToAdd(), round, haveTime)
	if err != nil {
		return nil, err
	}

	return miniBlocks, nil
}

// CommitBlock commits the block in the blockchain if everything was checked successfully
func (sp *shardProcessor) CommitBlock(
	chainHandler data.ChainHandler,
	headerHandler data.HeaderHandler,
	bodyHandler data.BodyHandler,
) error {

	var err error
	defer func() {
		if err != nil {
			sp.RevertAccountState()
		}
	}()

	err = checkForNils(chainHandler, headerHandler, bodyHandler)
	if err != nil {
		return err
	}

	log.Debug(fmt.Sprintf("started committing block with round %d and nonce %d\n",
		headerHandler.GetRound(),
		headerHandler.GetNonce()))

	err = sp.checkBlockValidity(chainHandler, headerHandler, bodyHandler)
	if err != nil {
		return err
	}

	header, ok := headerHandler.(*block.Header)
	if !ok {
		err = process.ErrWrongTypeAssertion
		return err
	}

	buff, err := sp.marshalizer.Marshal(header)
	if err != nil {
		return err
	}

	headerHash := sp.hasher.Compute(string(buff))
	nonceToByteSlice := sp.uint64Converter.ToByteSlice(header.Nonce)
	hdrNonceHashDataUnit := dataRetriever.ShardHdrNonceHashDataUnit + dataRetriever.UnitType(header.ShardId)

	errNotCritical := sp.store.Put(hdrNonceHashDataUnit, nonceToByteSlice, headerHash)
	log.LogIfError(errNotCritical)

	errNotCritical = sp.store.Put(dataRetriever.BlockHeaderUnit, headerHash, buff)
	log.LogIfError(errNotCritical)

	headerNoncePool := sp.dataPool.HeadersNonces()
	if headerNoncePool == nil {
		err = process.ErrNilDataPoolHolder
		return err
	}

	//TODO: Should be analyzed if put in pool is really necessary or not (right now there is no action of removing them)
	syncMap := &dataPool.ShardIdHashSyncMap{}
	syncMap.Store(headerHandler.GetShardID(), headerHash)
	headerNoncePool.Merge(headerHandler.GetNonce(), syncMap)

	body, ok := bodyHandler.(block.Body)
	if !ok {
		err = process.ErrWrongTypeAssertion
		return err
	}

	err = sp.txCoordinator.SaveBlockDataToStorage(body)
	if err != nil {
		return err
	}

	for i := 0; i < len(body); i++ {
		buff, err = sp.marshalizer.Marshal(body[i])
		if err != nil {
			return err
		}

		miniBlockHash := sp.hasher.Compute(string(buff))
		errNotCritical = sp.store.Put(dataRetriever.MiniBlockUnit, miniBlockHash, buff)
		log.LogIfError(errNotCritical)
	}

	processedMetaHdrs, err := sp.getProcessedMetaBlocksFromHeader(header)
	if err != nil {
		return err
	}

	finalHeaders, finalHeadersHashes, err := sp.getHighestHdrForOwnShardFromMetachain(processedMetaHdrs)
	if err != nil {
		return err
	}

	err = sp.saveLastNotarizedHeader(sharding.MetachainShardId, processedMetaHdrs)
	if err != nil {
		return err
	}

	headerMeta, err := sp.getLastNotarizedHdr(sharding.MetachainShardId)
	if err != nil {
		return err
	}

	sp.appStatusHandler.SetStringValue(core.MetricCrossCheckBlockHeight, fmt.Sprintf("meta %d", headerMeta.GetNonce()))

	_, err = sp.accounts.Commit()
	if err != nil {
		return err
	}

	log.Info(fmt.Sprintf("shard block with nonce %d and hash %s has been committed successfully\n",
		header.Nonce,
		core.ToB64(headerHash)))

	errNotCritical = sp.txCoordinator.RemoveBlockDataFromPool(body)
	if errNotCritical != nil {
		log.Debug(errNotCritical.Error())
	}

	errNotCritical = sp.removeProcessedMetaBlocksFromPool(processedMetaHdrs)
	if errNotCritical != nil {
		log.Debug(errNotCritical.Error())
	}

	errNotCritical = sp.forkDetector.AddHeader(header, headerHash, process.BHProcessed, finalHeaders, finalHeadersHashes)
	if errNotCritical != nil {
		log.Debug(errNotCritical.Error())
	}

	highestFinalBlockNonce := sp.forkDetector.GetHighestFinalBlockNonce()
	log.Info(fmt.Sprintf("shard block with nonce %d is the highest final block in shard %d\n",
		highestFinalBlockNonce,
		sp.shardCoordinator.SelfId()))

	sp.appStatusHandler.SetStringValue(core.MetricCurrentBlockHash, core.ToB64(headerHash))
	sp.appStatusHandler.SetUInt64Value(core.MetricHighestFinalBlockInShard, highestFinalBlockNonce)

	hdrsToAttestPreviousFinal := uint32(header.Nonce-highestFinalBlockNonce) + 1
	sp.removeNotarizedHdrsBehindPreviousFinal(hdrsToAttestPreviousFinal)

	err = chainHandler.SetCurrentBlockBody(body)
	if err != nil {
		return err
	}

	err = chainHandler.SetCurrentBlockHeader(header)
	if err != nil {
		return err
	}

	chainHandler.SetCurrentBlockHeaderHash(headerHash)
	sp.indexBlockIfNeeded(bodyHandler, headerHandler)

	go sp.cleanTxsPools()

	// write data to log
	go sp.txCounter.displayLogInfo(
		header,
		body,
		headerHash,
		sp.shardCoordinator.NumberOfShards(),
		sp.shardCoordinator.SelfId(),
		sp.dataPool,
	)

	sp.blockSizeThrottler.Succeed(header.Round)

	return nil
}

func (sp *shardProcessor) cleanTxsPools() {
	_, err := sp.txsPoolsCleaner.Clean(maxCleanTime)
	log.LogIfError(err)
	log.Info(fmt.Sprintf("%d txs have been removed from pools after cleaning\n", sp.txsPoolsCleaner.NumRemovedTxs()))
}

// getHighestHdrForOwnShardFromMetachain calculates the highest shard header notarized by metachain
func (sp *shardProcessor) getHighestHdrForOwnShardFromMetachain(
	processedHdrs []data.HeaderHandler,
) ([]data.HeaderHandler, [][]byte, error) {

	ownShIdHdrs := make([]data.HeaderHandler, 0)

	sort.Slice(processedHdrs, func(i, j int) bool {
		return processedHdrs[i].GetNonce() < processedHdrs[j].GetNonce()
	})

	for i := 0; i < len(processedHdrs); i++ {
		hdr, ok := processedHdrs[i].(*block.MetaBlock)
		if !ok {
			return nil, nil, process.ErrWrongTypeAssertion
		}

		hdrs, err := sp.getHighestHdrForShardFromMetachain(sp.shardCoordinator.SelfId(), hdr)
		if err != nil {
			return nil, nil, err
		}

		ownShIdHdrs = append(ownShIdHdrs, hdrs...)
	}

	if len(ownShIdHdrs) == 0 {
		ownShIdHdrs = append(ownShIdHdrs, &block.Header{})
	}

	sort.Slice(ownShIdHdrs, func(i, j int) bool {
		return ownShIdHdrs[i].GetNonce() < ownShIdHdrs[j].GetNonce()
	})

	ownShIdHdrsHashes := make([][]byte, 0)
	for i := 0; i < len(ownShIdHdrs); i++ {
		hash, _ := core.CalculateHash(sp.marshalizer, sp.hasher, ownShIdHdrs[i])
		ownShIdHdrsHashes = append(ownShIdHdrsHashes, hash)
	}

	return ownShIdHdrs, ownShIdHdrsHashes, nil
}

func (sp *shardProcessor) getHighestHdrForShardFromMetachain(shardId uint32, hdr *block.MetaBlock) ([]data.HeaderHandler, error) {
	ownShIdHdr := make([]data.HeaderHandler, 0)

	var errFound error
	// search for own shard id in shardInfo from metaHeaders
	for _, shardInfo := range hdr.ShardInfo {
		if shardInfo.ShardId != shardId {
			continue
		}

		ownHdr, err := process.GetShardHeader(shardInfo.HeaderHash, sp.dataPool.Headers(), sp.marshalizer, sp.store)
		if err != nil {
			go sp.onRequestHeaderHandler(shardInfo.ShardId, shardInfo.HeaderHash)

			log.Info(fmt.Sprintf("requested missing shard header with hash %s for shard %d\n",
				core.ToB64(shardInfo.HeaderHash),
				shardInfo.ShardId))

			errFound = err
			continue
		}

		ownShIdHdr = append(ownShIdHdr, ownHdr)
	}

	if errFound != nil {
		return nil, errFound
	}

	return ownShIdHdr, nil
}

// getProcessedMetaBlocksFromHeader returns all the meta blocks fully processed
func (sp *shardProcessor) getProcessedMetaBlocksFromHeader(header *block.Header) ([]data.HeaderHandler, error) {
	if header == nil {
		return nil, process.ErrNilBlockHeader
	}

	miniBlockHashes := make(map[int][]byte, 0)
	for i := 0; i < len(header.MiniBlockHeaders); i++ {
		miniBlockHashes[i] = header.MiniBlockHeaders[i].Hash
	}

	log.Debug(fmt.Sprintf("cross mini blocks in body: %d\n", len(miniBlockHashes)))

	processedMetaBlocks, processedCrossMiniBlocksHashes, err := sp.getProcessedMetaBlocksFromMiniBlockHashes(miniBlockHashes)
	if err != nil {
		return nil, err
	}

	sp.hdrsForCurrBlock.mutHdrsForBlock.RLock()
	for metaBlockHash, hdrInfo := range sp.hdrsForCurrBlock.hdrHashAndInfo {
		if !hdrInfo.usedInBlock {
			continue
		}

		metaBlock, ok := hdrInfo.hdr.(*block.MetaBlock)
		if !ok {
			sp.hdrsForCurrBlock.mutHdrsForBlock.RUnlock()
			return nil, process.ErrWrongTypeAssertion
		}

		crossMiniBlockHashes := metaBlock.GetMiniBlockHeadersWithDst(sp.shardCoordinator.SelfId())
		for hash := range crossMiniBlockHashes {
			if processedCrossMiniBlocksHashes[hash] {
				sp.addProcessedMiniBlock([]byte(metaBlockHash), []byte(hash))
			}
		}
	}
	sp.hdrsForCurrBlock.mutHdrsForBlock.RUnlock()

	return processedMetaBlocks, nil
}

// getProcessedMetaBlocks returns all the meta blocks fully processed
func (sp *shardProcessor) getProcessedMetaBlocksFromMiniBlocks(
	usedMiniBlocks []*block.MiniBlock,
) ([]data.HeaderHandler, error) {

	nrMiniBlocksUsed := len(usedMiniBlocks)

	sp.hdrsForCurrBlock.mutHdrsForBlock.RLock()
	nrMetaBlocksUsed := len(sp.hdrsForCurrBlock.hdrHashAndInfo)
	sp.hdrsForCurrBlock.mutHdrsForBlock.RUnlock()

	if nrMiniBlocksUsed == 0 || nrMetaBlocksUsed == 0 {
		// not an error, it can happen that no metablock hdr or no miniblock is used.
		return make([]data.HeaderHandler, 0), nil
	}

	miniBlockHashes := make(map[int][]byte, 0)
	for i := 0; i < len(usedMiniBlocks); i++ {
		if usedMiniBlocks[i].SenderShardID == sp.shardCoordinator.SelfId() {
			continue
		}

		miniBlockHash, err := core.CalculateHash(sp.marshalizer, sp.hasher, usedMiniBlocks[i])
		if err != nil {
			log.Debug(err.Error())
			continue
		}

		miniBlockHashes[i] = miniBlockHash
	}

	log.Debug(fmt.Sprintf("cross mini blocks in body: %d\n", len(miniBlockHashes)))
	processedMetaBlocks, _, err := sp.getProcessedMetaBlocksFromMiniBlockHashes(miniBlockHashes)

	return processedMetaBlocks, err
}

func (sp *shardProcessor) getProcessedMetaBlocksFromMiniBlockHashes(
	miniBlockHashes map[int][]byte,
) ([]data.HeaderHandler, map[string]bool, error) {

	processedMetaHdrs := make([]data.HeaderHandler, 0)
	processedCrossMiniBlocksHashes := make(map[string]bool)

	sp.hdrsForCurrBlock.mutHdrsForBlock.RLock()
	for metaBlockHash, hdrInfo := range sp.hdrsForCurrBlock.hdrHashAndInfo {
		if !hdrInfo.usedInBlock {
			continue
		}

		metaBlock, ok := hdrInfo.hdr.(*block.MetaBlock)
		if !ok {
			sp.hdrsForCurrBlock.mutHdrsForBlock.RUnlock()
			return nil, nil, process.ErrWrongTypeAssertion
		}

		log.Debug(fmt.Sprintf("meta header nonce: %d\n", metaBlock.Nonce))

		crossMiniBlockHashes := metaBlock.GetMiniBlockHeadersWithDst(sp.shardCoordinator.SelfId())
		for hash := range crossMiniBlockHashes {
			processedCrossMiniBlocksHashes[hash] = sp.isMiniBlockProcessed([]byte(metaBlockHash), []byte(hash))
		}

		for key, miniBlockHash := range miniBlockHashes {
			_, ok = crossMiniBlockHashes[string(miniBlockHash)]
			if !ok {
				continue
			}

			processedCrossMiniBlocksHashes[string(miniBlockHash)] = true

			delete(miniBlockHashes, key)
		}

		log.Debug(fmt.Sprintf("cross mini blocks in meta header: %d\n", len(crossMiniBlockHashes)))

		processedAll := true
		for hash := range crossMiniBlockHashes {
			if !processedCrossMiniBlocksHashes[hash] {
				processedAll = false
				break
			}
		}

		if processedAll {
			processedMetaHdrs = append(processedMetaHdrs, metaBlock)
		}
	}
	sp.hdrsForCurrBlock.mutHdrsForBlock.RUnlock()

	if len(processedMetaHdrs) > 1 {
		sort.Slice(processedMetaHdrs, func(i, j int) bool {
			return processedMetaHdrs[i].GetNonce() < processedMetaHdrs[j].GetNonce()
		})
	}

	return processedMetaHdrs, processedCrossMiniBlocksHashes, nil
}

func (sp *shardProcessor) removeProcessedMetaBlocksFromPool(processedMetaHdrs []data.HeaderHandler) error {
	lastNotarizedMetaHdr, err := sp.getLastNotarizedHdr(sharding.MetachainShardId)
	if err != nil {
		return err
	}

	processed := 0
	// processedMetaHdrs is also sorted
	for i := 0; i < len(processedMetaHdrs); i++ {
		hdr := processedMetaHdrs[i]

		// remove process finished
		if hdr.GetNonce() > lastNotarizedMetaHdr.GetNonce() {
			continue
		}

		// metablock was processed and finalized
		buff, err := sp.marshalizer.Marshal(hdr)
		if err != nil {
			log.Error(err.Error())
			continue
		}

		headerHash := sp.hasher.Compute(string(buff))
		nonceToByteSlice := sp.uint64Converter.ToByteSlice(hdr.GetNonce())
		err = sp.store.Put(dataRetriever.MetaHdrNonceHashDataUnit, nonceToByteSlice, headerHash)
		if err != nil {
			log.Error(err.Error())
			continue
		}

		err = sp.store.Put(dataRetriever.MetaBlockUnit, headerHash, buff)
		if err != nil {
			log.Error(err.Error())
			continue
		}

		sp.dataPool.MetaBlocks().Remove(headerHash)
		sp.dataPool.HeadersNonces().Remove(hdr.GetNonce(), sharding.MetachainShardId)
		sp.removeAllProcessedMiniBlocks(headerHash)

		log.Debug(fmt.Sprintf("metaBlock with round %d nonce %d and hash %s has been processed completely and removed from pool\n",
			hdr.GetRound(),
			hdr.GetNonce(),
			core.ToB64(headerHash)))

		processed++
	}

	if processed > 0 {
		log.Debug(fmt.Sprintf("%d meta blocks have been processed completely and removed from pool\n", processed))
	}

	return nil
}

// receivedMetaBlock is a callback function when a new metablock was received
// upon receiving, it parses the new metablock and requests miniblocks and transactions
// which destination is the current shard
func (sp *shardProcessor) receivedMetaBlock(metaBlockHash []byte) {
	metaBlockPool := sp.dataPool.MetaBlocks()
	if metaBlockPool == nil {
		return
	}

	obj, ok := metaBlockPool.Peek(metaBlockHash)
	if !ok {
		return
	}

	metaBlock, ok := obj.(*block.MetaBlock)
	if !ok {
		return
	}

	log.Debug(fmt.Sprintf("received metablock with hash %s and nonce %d from network\n",
		core.ToB64(metaBlockHash),
		metaBlock.Nonce))

	sp.hdrsForCurrBlock.mutHdrsForBlock.Lock()

	if sp.hdrsForCurrBlock.missingHdrs > 0 || sp.hdrsForCurrBlock.missingFinalHdrs > 0 {
		hdrInfoForHash := sp.hdrsForCurrBlock.hdrHashAndInfo[string(metaBlockHash)]
		if hdrInfoForHash != nil && (hdrInfoForHash.hdr == nil || hdrInfoForHash.hdr.IsInterfaceNil()) {
			hdrInfoForHash.hdr = metaBlock
			sp.hdrsForCurrBlock.missingHdrs--

			if metaBlock.Nonce > sp.currHighestMetaHdrNonce {
				sp.currHighestMetaHdrNonce = metaBlock.Nonce
			}
		}

		if sp.hdrsForCurrBlock.missingHdrs == 0 {
			missingFinalHdrs := sp.hdrsForCurrBlock.missingFinalHdrs
			sp.hdrsForCurrBlock.missingFinalHdrs = sp.requestFinalMissingHeaders()
			if sp.hdrsForCurrBlock.missingFinalHdrs == 0 {
				log.Info(fmt.Sprintf("received %d missing final meta headers\n", missingFinalHdrs))
			} else {
				log.Info(fmt.Sprintf("requested %d missing final meta headers\n", sp.hdrsForCurrBlock.missingFinalHdrs))
			}
		}

		missingHdrs := sp.hdrsForCurrBlock.missingHdrs
		missingFinalHdrs := sp.hdrsForCurrBlock.missingFinalHdrs
		sp.hdrsForCurrBlock.mutHdrsForBlock.Unlock()

		allMissingNeededHdrsReceived := missingHdrs == 0 && missingFinalHdrs == 0
		if allMissingNeededHdrsReceived {
			sp.chRcvAllMetaHdrs <- true
		}
	} else {
		sp.hdrsForCurrBlock.mutHdrsForBlock.Unlock()
	}

	lastNotarizedHdr, err := sp.getLastNotarizedHdr(sharding.MetachainShardId)
	if err != nil {
		return
	}
	if metaBlock.GetNonce() <= lastNotarizedHdr.GetNonce() {
		return
	}
	if metaBlock.GetRound() <= lastNotarizedHdr.GetRound() {
		return
	}

	sp.txCoordinator.RequestMiniBlocks(metaBlock)
}

// requestFinalMissingHeaders requests the headers needed to accept the current selected headers for processing the
// current block. It requests the metaBlockFinality headers greater than the highest meta header related to the block
// which should be processed
func (sp *shardProcessor) requestFinalMissingHeaders() uint32 {
	requestedBlockHeaders := uint32(0)
	for i := sp.currHighestMetaHdrNonce + 1; i <= sp.currHighestMetaHdrNonce+uint64(sp.metaBlockFinality); i++ {
		if sp.currHighestMetaHdrNonce == uint64(0) {
			continue
		}

		metaBlock, metaBlockHash, err := process.GetMetaHeaderFromPoolWithNonce(
			i,
			sp.dataPool.MetaBlocks(),
			sp.dataPool.HeadersNonces())

		if err != nil {
			requestedBlockHeaders++
			go sp.onRequestHeaderHandlerByNonce(sharding.MetachainShardId, i)
			continue
		}

		sp.hdrsForCurrBlock.hdrHashAndInfo[string(metaBlockHash)] = &hdrInfo{hdr: metaBlock, usedInBlock: false}
	}

	return requestedBlockHeaders
}

func (sp *shardProcessor) requestMetaHeaders(header *block.Header) (uint32, uint32) {
	_ = process.EmptyChannel(sp.chRcvAllMetaHdrs)

	if len(header.MetaBlockHashes) == 0 {
		return 0, 0
	}

	missingHeadersHashes := sp.computeMissingAndExistingMetaHeaders(header)

	sp.hdrsForCurrBlock.mutHdrsForBlock.Lock()
	for _, hash := range missingHeadersHashes {
		sp.hdrsForCurrBlock.hdrHashAndInfo[string(hash)] = &hdrInfo{hdr: nil, usedInBlock: true}
		go sp.onRequestHeaderHandler(sharding.MetachainShardId, hash)
	}

	if sp.hdrsForCurrBlock.missingHdrs == 0 {
		sp.hdrsForCurrBlock.missingFinalHdrs = sp.requestFinalMissingHeaders()
	}

	requestedHdrs := sp.hdrsForCurrBlock.missingHdrs
	requestedFinalHdrs := sp.hdrsForCurrBlock.missingFinalHdrs
	sp.hdrsForCurrBlock.mutHdrsForBlock.Unlock()

	return requestedHdrs, requestedFinalHdrs
}

func (sp *shardProcessor) computeMissingAndExistingMetaHeaders(header *block.Header) [][]byte {
	missingHeadersHashes := make([][]byte, 0)
	sp.currHighestMetaHdrNonce = uint64(0)

	sp.hdrsForCurrBlock.mutHdrsForBlock.Lock()
	for i := 0; i < len(header.MetaBlockHashes); i++ {
		hdr, err := process.GetMetaHeaderFromPool(
			header.MetaBlockHashes[i],
			sp.dataPool.MetaBlocks())

		if err != nil {
			missingHeadersHashes = append(missingHeadersHashes, header.MetaBlockHashes[i])
			sp.hdrsForCurrBlock.missingHdrs++
			continue
		}

		sp.hdrsForCurrBlock.hdrHashAndInfo[string(header.MetaBlockHashes[i])] = &hdrInfo{hdr: hdr, usedInBlock: true}

		if hdr.Nonce > sp.currHighestMetaHdrNonce {
			sp.currHighestMetaHdrNonce = hdr.Nonce
		}
	}
	sp.hdrsForCurrBlock.mutHdrsForBlock.Unlock()

	return missingHeadersHashes
}

func (sp *shardProcessor) verifyCrossShardMiniBlockDstMe(hdr *block.Header) error {
	miniBlockMetaHashes, err := sp.getAllMiniBlockDstMeFromMeta(hdr.Round)
	if err != nil {
		return err
	}

	crossMiniBlockHashes := hdr.GetMiniBlockHeadersWithDst(sp.shardCoordinator.SelfId())
	for hash := range crossMiniBlockHashes {
		if _, ok := miniBlockMetaHashes[hash]; !ok {
			return process.ErrCrossShardMBWithoutConfirmationFromMeta
		}
	}

	return nil
}

func (sp *shardProcessor) getAllMiniBlockDstMeFromMeta(round uint64) (map[string][]byte, error) {
	lastHdr, err := sp.getLastNotarizedHdr(sharding.MetachainShardId)
	if err != nil {
		return nil, err
	}

	miniBlockMetaHashes := make(map[string][]byte)

	sp.hdrsForCurrBlock.mutHdrsForBlock.RLock()
	for metaBlockHash, hdrInfo := range sp.hdrsForCurrBlock.hdrHashAndInfo {
		if !hdrInfo.usedInBlock {
			continue
		}
		hdr, ok := hdrInfo.hdr.(*block.MetaBlock)
		if !ok {
			continue
		}
		if hdr.GetRound() > round {
			continue
		}
		if hdr.GetRound() <= lastHdr.GetRound() {
			continue
		}
		if hdr.GetNonce() <= lastHdr.GetNonce() {
			continue
		}

		crossMiniBlockHashes := hdr.GetMiniBlockHeadersWithDst(sp.shardCoordinator.SelfId())
		for hash := range crossMiniBlockHashes {
			miniBlockMetaHashes[hash] = []byte(metaBlockHash)
		}
	}
	sp.hdrsForCurrBlock.mutHdrsForBlock.RUnlock()

	return miniBlockMetaHashes, nil
}

func (sp *shardProcessor) getOrderedMetaBlocks(round uint64) ([]*hashAndHdr, error) {
	metaBlocksPool := sp.dataPool.MetaBlocks()
	if metaBlocksPool == nil {
		return nil, process.ErrNilMetaBlockPool
	}

	lastHdr, err := sp.getLastNotarizedHdr(sharding.MetachainShardId)
	if err != nil {
		return nil, err
	}

	orderedMetaBlocks := make([]*hashAndHdr, 0)
	for _, key := range metaBlocksPool.Keys() {
		val, _ := metaBlocksPool.Peek(key)
		if val == nil {
			continue
		}

		hdr, ok := val.(*block.MetaBlock)
		if !ok {
			continue
		}

		if hdr.GetRound() > round {
			continue
		}
		if hdr.GetRound() <= lastHdr.GetRound() {
			continue
		}
		if hdr.GetNonce() <= lastHdr.GetNonce() {
			continue
		}

		orderedMetaBlocks = append(orderedMetaBlocks, &hashAndHdr{hdr: hdr, hash: key})
	}

	if len(orderedMetaBlocks) > 1 {
		sort.Slice(orderedMetaBlocks, func(i, j int) bool {
			return orderedMetaBlocks[i].hdr.GetNonce() < orderedMetaBlocks[j].hdr.GetNonce()
		})
	}

	return orderedMetaBlocks, nil
}

// isMetaHeaderFinal verifies if meta is trully final, in order to not do rollbacks
func (sp *shardProcessor) isMetaHeaderFinal(currHdr data.HeaderHandler, sortedHdrs []*hashAndHdr, startPos int) bool {
	if currHdr == nil || currHdr.IsInterfaceNil() {
		return false
	}
	if sortedHdrs == nil {
		return false
	}

	// verify if there are "K" block after current to make this one final
	lastVerifiedHdr := currHdr
	nextBlocksVerified := 0

	for i := startPos; i < len(sortedHdrs); i++ {
		if nextBlocksVerified >= sp.metaBlockFinality {
			return true
		}

		// found a header with the next nonce
		tmpHdr := sortedHdrs[i].hdr
		if tmpHdr.GetNonce() == lastVerifiedHdr.GetNonce()+1 {
			err := sp.isHdrConstructionValid(tmpHdr, lastVerifiedHdr)
			if err != nil {
				continue
			}

			lastVerifiedHdr = tmpHdr
			nextBlocksVerified += 1
		}
	}

	if nextBlocksVerified >= sp.metaBlockFinality {
		return true
	}

	return false
}

// full verification through metachain header
func (sp *shardProcessor) createAndProcessCrossMiniBlocksDstMe(
	noShards uint32,
	maxItemsInBlock uint32,
	round uint64,
	haveTime func() bool,
) (block.MiniBlockSlice, uint32, uint32, error) {

	miniBlocks := make(block.MiniBlockSlice, 0)
	txsAdded := uint32(0)
	hdrsAdded := uint32(0)

	orderedMetaBlocks, err := sp.getOrderedMetaBlocks(round)
	if err != nil {
		return nil, 0, 0, err
	}

	log.Info(fmt.Sprintf("meta blocks ordered: %d\n", len(orderedMetaBlocks)))

	lastMetaHdr, err := sp.getLastNotarizedHdr(sharding.MetachainShardId)
	if err != nil {
		return nil, 0, 0, err
	}

	// do processing in order
	sp.hdrsForCurrBlock.mutHdrsForBlock.Lock()
	for i := 0; i < len(orderedMetaBlocks); i++ {
		if !haveTime() {
			log.Info(fmt.Sprintf("time is up after putting %d cross txs with destination to current shard\n", txsAdded))
			break
		}

		if len(miniBlocks) >= core.MaxMiniBlocksInBlock {
			log.Info(fmt.Sprintf("%d max number of mini blocks allowed to be added in one shard block has been reached\n", len(miniBlocks)))
			break
		}

		itemsAddedInHeader := uint32(len(sp.hdrsForCurrBlock.hdrHashAndInfo) + len(miniBlocks))
		if itemsAddedInHeader >= maxItemsInBlock {
			log.Info(fmt.Sprintf("%d max records allowed to be added in shard header has been reached\n", maxItemsInBlock))
			break
		}

		hdr, ok := orderedMetaBlocks[i].hdr.(*block.MetaBlock)
		if !ok {
			continue
		}

		err = sp.isHdrConstructionValid(hdr, lastMetaHdr)
		if err != nil {
			continue
		}

		isFinal := sp.isMetaHeaderFinal(hdr, orderedMetaBlocks, i+1)
		if !isFinal {
			continue
		}

		if len(hdr.GetMiniBlockHeadersWithDst(sp.shardCoordinator.SelfId())) == 0 {
			sp.hdrsForCurrBlock.hdrHashAndInfo[string(orderedMetaBlocks[i].hash)] = &hdrInfo{hdr: hdr, usedInBlock: true}
			hdrsAdded++
			lastMetaHdr = hdr
			continue
		}

		itemsAddedInBody := txsAdded
		if itemsAddedInBody >= maxItemsInBlock {
			continue
		}

		maxTxSpaceRemained := int32(maxItemsInBlock) - int32(itemsAddedInBody)
		maxMbSpaceRemained := sp.getMaxMiniBlocksSpaceRemained(
			maxItemsInBlock,
			itemsAddedInHeader+1,
			uint32(len(miniBlocks)))

		if maxTxSpaceRemained > 0 && maxMbSpaceRemained > 0 {
			processedMiniBlocksHashes := sp.getProcessedMiniBlocksHashes(orderedMetaBlocks[i].hash)
			currMBProcessed, currTxsAdded, hdrProcessFinished := sp.txCoordinator.CreateMbsAndProcessCrossShardTransactionsDstMe(
				hdr,
				processedMiniBlocksHashes,
				uint32(maxTxSpaceRemained),
				uint32(maxMbSpaceRemained),
				round,
				haveTime)

			// all txs processed, add to processed miniblocks
			miniBlocks = append(miniBlocks, currMBProcessed...)
			txsAdded = txsAdded + currTxsAdded

			if currTxsAdded > 0 {
				sp.hdrsForCurrBlock.hdrHashAndInfo[string(orderedMetaBlocks[i].hash)] = &hdrInfo{hdr: hdr, usedInBlock: true}
				hdrsAdded++
			}

			if !hdrProcessFinished {
				break
			}

			lastMetaHdr = hdr
		}
	}
	sp.hdrsForCurrBlock.mutHdrsForBlock.Unlock()

	return miniBlocks, txsAdded, hdrsAdded, nil
}

func (sp *shardProcessor) createMiniBlocks(
	noShards uint32,
	maxItemsInBlock uint32,
	round uint64,
	haveTime func() bool,
) (block.Body, error) {

	miniBlocks := make(block.Body, 0)

	if sp.accounts.JournalLen() != 0 {
		return nil, process.ErrAccountStateDirty
	}

	if !haveTime() {
		log.Info(fmt.Sprintf("time is up after entered in createMiniBlocks method\n"))
		return nil, process.ErrTimeIsOut
	}

	txPool := sp.dataPool.Transactions()
	if txPool == nil {
		return nil, process.ErrNilTransactionPool
	}

	destMeMiniBlocks, txs, hdrs, err := sp.createAndProcessCrossMiniBlocksDstMe(noShards, maxItemsInBlock, round, haveTime)
	if err != nil {
		log.Info(err.Error())
	}

	processedMetaHdrs, errNotCritical := sp.getProcessedMetaBlocksFromMiniBlocks(destMeMiniBlocks)
	if errNotCritical != nil {
		log.Debug(errNotCritical.Error())
	}

	err = sp.setMetaConsensusData(processedMetaHdrs)
	if err != nil {
		return nil, err
	}

	log.Info(fmt.Sprintf("processed %d miniblocks and %d txs with destination in self shard\n", len(destMeMiniBlocks), txs))

	if len(destMeMiniBlocks) > 0 {
		miniBlocks = append(miniBlocks, destMeMiniBlocks...)
	}

	maxTxSpaceRemained := int32(maxItemsInBlock) - int32(txs)
	maxMbSpaceRemained := sp.getMaxMiniBlocksSpaceRemained(
		maxItemsInBlock,
		uint32(len(destMeMiniBlocks))+hdrs,
		uint32(len(miniBlocks)))

	if maxTxSpaceRemained > 0 && maxMbSpaceRemained > 0 {
		mbFromMe := sp.txCoordinator.CreateMbsAndProcessTransactionsFromMe(
			uint32(maxTxSpaceRemained),
			uint32(maxMbSpaceRemained),
			round,
			haveTime)

		if len(mbFromMe) > 0 {
			miniBlocks = append(miniBlocks, mbFromMe...)
		}
	}

	log.Info(fmt.Sprintf("creating mini blocks has been finished: created %d mini blocks\n", len(miniBlocks)))
	return miniBlocks, nil
}

// CreateBlockHeader creates a miniblock header list given a block body
func (sp *shardProcessor) CreateBlockHeader(bodyHandler data.BodyHandler, round uint64, haveTime func() bool) (data.HeaderHandler, error) {
	log.Debug(fmt.Sprintf("started creating block header in round %d\n", round))
	header := &block.Header{
		MiniBlockHeaders: make([]block.MiniBlockHeader, 0),
		RootHash:         sp.getRootHash(),
		ShardId:          sp.shardCoordinator.SelfId(),
		PrevRandSeed:     make([]byte, 0),
		RandSeed:         make([]byte, 0),
	}

	defer func() {
		go sp.checkAndRequestIfMetaHeadersMissing(round)
	}()

	if bodyHandler == nil || bodyHandler.IsInterfaceNil() {
		return header, nil
	}

	body, ok := bodyHandler.(block.Body)
	if !ok {
		return nil, process.ErrWrongTypeAssertion
	}

	totalTxCount := 0
	miniBlockHeaders := make([]block.MiniBlockHeader, len(body))

	for i := 0; i < len(body); i++ {
		txCount := len(body[i].TxHashes)
		totalTxCount += txCount

		miniBlockHash, err := core.CalculateHash(sp.marshalizer, sp.hasher, body[i])
		if err != nil {
			return nil, err
		}

		miniBlockHeaders[i] = block.MiniBlockHeader{
			Hash:            miniBlockHash,
			SenderShardID:   body[i].SenderShardID,
			ReceiverShardID: body[i].ReceiverShardID,
			TxCount:         uint32(txCount),
			Type:            body[i].Type,
		}
	}

	header.MiniBlockHeaders = miniBlockHeaders
	header.TxCount = uint32(totalTxCount)

	sp.appStatusHandler.SetUInt64Value(core.MetricNumTxInBlock, uint64(totalTxCount))
	sp.appStatusHandler.SetUInt64Value(core.MetricNumMiniBlocks, uint64(len(body)))

	header.MetaBlockHashes = sp.sortHdrsHashesForCurrentBlock(true)

	sp.blockSizeThrottler.Add(
		round,
		uint32(core.Max(int32(header.ItemsInBody()), int32(header.ItemsInHeader()))))

	return header, nil
}

func (sp *shardProcessor) waitForMetaHdrHashes(waitTime time.Duration) error {
	select {
	case <-sp.chRcvAllMetaHdrs:
		return nil
	case <-time.After(waitTime):
		return process.ErrTimeIsOut
	}
}

// MarshalizedDataToBroadcast prepares underlying data into a marshalized object according to destination
func (sp *shardProcessor) MarshalizedDataToBroadcast(
	header data.HeaderHandler,
	bodyHandler data.BodyHandler,
) (map[uint32][]byte, map[string][][]byte, error) {

	if bodyHandler == nil || bodyHandler.IsInterfaceNil() {
		return nil, nil, process.ErrNilMiniBlocks
	}

	body, ok := bodyHandler.(block.Body)
	if !ok {
		return nil, nil, process.ErrWrongTypeAssertion
	}

	mrsData := make(map[uint32][]byte)
	bodies, mrsTxs := sp.txCoordinator.CreateMarshalizedData(body)

	for shardId, subsetBlockBody := range bodies {
		buff, err := sp.marshalizer.Marshal(subsetBlockBody)
		if err != nil {
			log.Debug(process.ErrMarshalWithoutSuccess.Error())
			continue
		}
		mrsData[shardId] = buff
	}

	return mrsData, mrsTxs, nil
}

// DecodeBlockBody method decodes block body from a given byte array
func (sp *shardProcessor) DecodeBlockBody(dta []byte) data.BodyHandler {
	if dta == nil {
		return nil
	}

	var body block.Body

	err := sp.marshalizer.Unmarshal(&body, dta)
	if err != nil {
		log.Error(err.Error())
		return nil
	}

	return body
}

// DecodeBlockHeader method decodes block header from a given byte array
func (sp *shardProcessor) DecodeBlockHeader(dta []byte) data.HeaderHandler {
	if dta == nil {
		return nil
	}

	var header block.Header

	err := sp.marshalizer.Unmarshal(&header, dta)
	if err != nil {
		log.Error(err.Error())
		return nil
	}

	return &header
}

// IsInterfaceNil returns true if there is no value under the interface
func (sp *shardProcessor) IsInterfaceNil() bool {
	if sp == nil {
		return true
	}
	return false
}

func (sp *shardProcessor) addProcessedMiniBlock(metaBlockHash []byte, miniBlockHash []byte) {
	sp.mutProcessedMiniBlocks.Lock()
	miniBlocksProcessed, ok := sp.processedMiniBlocks[string(metaBlockHash)]
	if !ok {
		miniBlocksProcessed := make(map[string]struct{})
		miniBlocksProcessed[string(miniBlockHash)] = struct{}{}
		sp.processedMiniBlocks[string(metaBlockHash)] = miniBlocksProcessed
		sp.mutProcessedMiniBlocks.Unlock()
		return
	}

	miniBlocksProcessed[string(miniBlockHash)] = struct{}{}
	sp.mutProcessedMiniBlocks.Unlock()
}

func (sp *shardProcessor) removeProcessedMiniBlock(miniBlockHash []byte) {
	sp.mutProcessedMiniBlocks.Lock()
	for _, miniBlocksProcessed := range sp.processedMiniBlocks {
		_, isProcessed := miniBlocksProcessed[string(miniBlockHash)]
		if isProcessed {
			delete(miniBlocksProcessed, string(miniBlockHash))
		}
	}
	sp.mutProcessedMiniBlocks.Unlock()
}

func (sp *shardProcessor) removeAllProcessedMiniBlocks(metaBlockHash []byte) {
	sp.mutProcessedMiniBlocks.Lock()
	delete(sp.processedMiniBlocks, string(metaBlockHash))
	sp.mutProcessedMiniBlocks.Unlock()
}

func (sp *shardProcessor) getProcessedMiniBlocksHashes(metaBlockHash []byte) map[string]struct{} {
	sp.mutProcessedMiniBlocks.RLock()
	processedMiniBlocksHashes := sp.processedMiniBlocks[string(metaBlockHash)]
	sp.mutProcessedMiniBlocks.RUnlock()

	return processedMiniBlocksHashes
}

func (sp *shardProcessor) isMiniBlockProcessed(metaBlockHash []byte, miniBlockHash []byte) bool {
	sp.mutProcessedMiniBlocks.RLock()
	miniBlocksProcessed, ok := sp.processedMiniBlocks[string(metaBlockHash)]
	if !ok {
		sp.mutProcessedMiniBlocks.RUnlock()
		return false
	}

	_, isProcessed := miniBlocksProcessed[string(miniBlockHash)]
	sp.mutProcessedMiniBlocks.RUnlock()

	return isProcessed
}

func (sp *shardProcessor) getMaxMiniBlocksSpaceRemained(
	maxItemsInBlock uint32,
	itemsAddedInBlock uint32,
	miniBlocksAddedInBlock uint32,
) int32 {
	mbSpaceRemainedInBlock := int32(maxItemsInBlock) - int32(itemsAddedInBlock)
	mbSpaceRemainedInCache := int32(core.MaxMiniBlocksInBlock) - int32(miniBlocksAddedInBlock)
	maxMbSpaceRemained := core.Min(mbSpaceRemainedInBlock, mbSpaceRemainedInCache)

	return maxMbSpaceRemained
}

func (sp *shardProcessor) CreateBlockStarted() {
	sp.txCoordinator.CreateBlockStarted()

	sp.hdrsForCurrBlock.mutHdrsForBlock.Lock()
	sp.hdrsForCurrBlock.missingHdrs = 0
	sp.hdrsForCurrBlock.missingFinalHdrs = 0
	sp.hdrsForCurrBlock.hdrHashAndInfo = make(map[string]*hdrInfo)
	sp.hdrsForCurrBlock.mutHdrsForBlock.Unlock()
}

func (sp *shardProcessor) sortHdrsForCurrentBlock(usedInBlock bool) ([]*block.MetaBlock, error) {
	hdrsForCurrentBlock := make([]*block.MetaBlock, 0)

	sp.hdrsForCurrBlock.mutHdrsForBlock.RLock()
	for _, hdrInfo := range sp.hdrsForCurrBlock.hdrHashAndInfo {
		if hdrInfo.usedInBlock != usedInBlock {
			continue
		}

		metaHdr, ok := hdrInfo.hdr.(*block.MetaBlock)
		if !ok {
			sp.hdrsForCurrBlock.mutHdrsForBlock.RUnlock()
			return nil, process.ErrWrongTypeAssertion
		}

		hdrsForCurrentBlock = append(hdrsForCurrentBlock, metaHdr)
	}
	sp.hdrsForCurrBlock.mutHdrsForBlock.RUnlock()

	if len(hdrsForCurrentBlock) > 1 {
		sort.Slice(hdrsForCurrentBlock, func(i, j int) bool {
			return hdrsForCurrentBlock[i].Nonce < hdrsForCurrentBlock[j].Nonce
		})
	}

	return hdrsForCurrentBlock, nil
}

func (sp *shardProcessor) sortHdrsHashesForCurrentBlock(usedInBlock bool) [][]byte {
	hdrsForCurrentBlockInfo := make([]*nonceAndHashInfo, 0)

	sp.hdrsForCurrBlock.mutHdrsForBlock.RLock()
	for metaBlockHash, hdrInfo := range sp.hdrsForCurrBlock.hdrHashAndInfo {
		if hdrInfo.usedInBlock != usedInBlock {
			continue
		}

		hdrsForCurrentBlockInfo = append(hdrsForCurrentBlockInfo, &nonceAndHashInfo{nonce: hdrInfo.hdr.GetNonce(), hash: []byte(metaBlockHash)})
	}
	sp.hdrsForCurrBlock.mutHdrsForBlock.RUnlock()

	if len(hdrsForCurrentBlockInfo) > 1 {
		sort.Slice(hdrsForCurrentBlockInfo, func(i, j int) bool {
			return hdrsForCurrentBlockInfo[i].nonce < hdrsForCurrentBlockInfo[j].nonce
		})
	}

	hdrsHashesForCurrentBlock := make([][]byte, len(hdrsForCurrentBlockInfo))
	for i := 0; i < len(hdrsForCurrentBlockInfo); i++ {
		hdrsHashesForCurrentBlock[i] = hdrsForCurrentBlockInfo[i].hash
	}

	return hdrsHashesForCurrentBlock
}
