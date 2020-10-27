package node

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ElrondNetwork/elrond-go/config"
	"github.com/ElrondNetwork/elrond-go/core"
	"github.com/ElrondNetwork/elrond-go/core/accumulator"
	"github.com/ElrondNetwork/elrond-go/data/state"
	"github.com/ElrondNetwork/elrond-go/factory"
	"github.com/ElrondNetwork/elrond-go/node/nodeDebugFactory"
	factory4 "github.com/ElrondNetwork/elrond-go/process/factory"
	"github.com/ElrondNetwork/elrond-go/process/smartContract"
	"github.com/ElrondNetwork/elrond-go/process/throttle/antiflood/blackList"
	"github.com/ElrondNetwork/elrond-go/sharding"
	"github.com/ElrondNetwork/elrond-go/update"
	factory3 "github.com/ElrondNetwork/elrond-go/update/factory"
	"github.com/ElrondNetwork/elrond-go/update/trigger"
)

func CreateHardForkTrigger(
	config *config.Config,
	shardCoordinator sharding.Coordinator,
	nodesCoordinator sharding.NodesCoordinator,
	nodesShuffledOut update.Closer,
	coreData factory.CoreComponentsHolder,
	stateComponents factory.StateComponentsHolder,
	data factory.DataComponentsHolder,
	crypto factory.CryptoComponentsHolder,
	process factory.ProcessComponentsHolder,
	network factory.NetworkComponentsHolder,
	epochNotifier factory.EpochStartNotifierWithConfirm,
	importStartHandler update.ImportStartHandler,
	workingDir string,
) (HardforkTrigger, error) {

	selfPubKeyBytes := crypto.PublicKeyBytes()
	triggerPubKeyBytes, err := coreData.ValidatorPubKeyConverter().Decode(config.Hardfork.PublicKeyToListenFrom)
	if err != nil {
		return nil, fmt.Errorf("%w while decoding HardforkConfig.PublicKeyToListenFrom", err)
	}

	accountsDBs := make(map[state.AccountsDbIdentifier]state.AccountsAdapter)
	accountsDBs[state.UserAccountsState] = stateComponents.AccountsAdapter()
	accountsDBs[state.PeerAccountsState] = stateComponents.PeerAccounts()
	hardForkConfig := config.Hardfork
	exportFolder := filepath.Join(workingDir, hardForkConfig.ImportFolder)
	argsExporter := factory3.ArgsExporter{
		CoreComponents:           coreData,
		CryptoComponents:         crypto,
		HeaderValidator:          process.HeaderConstructionValidator(),
		DataPool:                 data.Datapool(),
		StorageService:           data.StorageService(),
		RequestHandler:           process.RequestHandler(),
		ShardCoordinator:         shardCoordinator,
		Messenger:                network.NetworkMessenger(),
		ActiveAccountsDBs:        accountsDBs,
		ExistingResolvers:        process.ResolversFinder(),
		ExportFolder:             exportFolder,
		ExportTriesStorageConfig: hardForkConfig.ExportTriesStorageConfig,
		ExportStateStorageConfig: hardForkConfig.ExportStateStorageConfig,
		ExportStateKeysConfig:    hardForkConfig.ExportKeysStorageConfig,
		WhiteListHandler:         process.WhiteListHandler(),
		WhiteListerVerifiedTxs:   process.WhiteListerVerifiedTxs(),
		InterceptorsContainer:    process.InterceptorsContainer(),
		NodesCoordinator:         nodesCoordinator,
		HeaderSigVerifier:        process.HeaderSigVerifier(),
		HeaderIntegrityVerifier:  process.HeaderIntegrityVerifier(),
		MaxTrieLevelInMemory:     config.StateTriesConfig.MaxStateTrieLevelInMemory,
		InputAntifloodHandler:    network.InputAntiFloodHandler(),
		OutputAntifloodHandler:   network.OutputAntiFloodHandler(),
		ValidityAttester:         process.BlockTracker(),
		Rounder:                  process.Rounder(),
	}
	hardForkExportFactory, err := factory3.NewExportHandlerFactory(argsExporter)
	if err != nil {
		return nil, err
	}

	atArgumentParser := smartContract.NewArgumentParser()
	argTrigger := trigger.ArgHardforkTrigger{
		TriggerPubKeyBytes:        triggerPubKeyBytes,
		SelfPubKeyBytes:           selfPubKeyBytes,
		Enabled:                   config.Hardfork.EnableTrigger,
		EnabledAuthenticated:      config.Hardfork.EnableTriggerFromP2P,
		ArgumentParser:            atArgumentParser,
		EpochProvider:             process.EpochStartTrigger(),
		ExportFactoryHandler:      hardForkExportFactory,
		ChanStopNodeProcess:       coreData.ChanStopNodeProcess(),
		EpochConfirmedNotifier:    epochNotifier,
		CloseAfterExportInMinutes: config.Hardfork.CloseAfterExportInMinutes,
		ImportStartHandler:        importStartHandler,
	}
	hardforkTrigger, err := trigger.NewTrigger(argTrigger)
	if err != nil {
		return nil, err
	}

	err = hardforkTrigger.AddCloser(nodesShuffledOut)
	if err != nil {
		return nil, fmt.Errorf("%w when adding nodeShufflerOut in hardForkTrigger", err)
	}

	return hardforkTrigger, nil
}

func getConsensusGroupSize(nodesConfig sharding.GenesisNodesSetupHandler, shardCoordinator sharding.Coordinator) (uint32, error) {
	if shardCoordinator.SelfId() == core.MetachainShardId {
		return nodesConfig.GetMetaConsensusGroupSize(), nil
	}
	if shardCoordinator.SelfId() < shardCoordinator.NumberOfShards() {
		return nodesConfig.GetShardConsensusGroupSize(), nil
	}

	return 0, state.ErrUnknownShardId
}

// prepareOpenTopics will set to the anti flood handler the topics for which
// the node can receive messages from others than validators
func prepareOpenTopics(
	antiflood factory.P2PAntifloodHandler,
	shardCoordinator sharding.Coordinator,
) {
	selfID := shardCoordinator.SelfId()
	if selfID == core.MetachainShardId {
		antiflood.SetTopicsForAll(core.HeartbeatTopic)
		return
	}

	selfShardTxTopic := factory4.TransactionTopic + core.CommunicationIdentifierBetweenShards(selfID, selfID)
	antiflood.SetTopicsForAll(core.HeartbeatTopic, selfShardTxTopic)
}

func CreateNode(
	config *config.Config,
	bootstrapComponents factory.BootstrapComponentsHandler,
	coreComponents factory.CoreComponentsHandler,
	cryptoComponents factory.CryptoComponentsHandler,
	dataComponents factory.DataComponentsHandler,
	networkComponents factory.NetworkComponentsHandler,
	processComponents factory.ProcessComponentsHandler,
	stateComponents factory.StateComponentsHandler,
	statusComponents factory.StatusComponentsHandler,
	heartbeatComponents factory.HeartbeatComponentsHandler,
	consensusComponents factory.ConsensusComponentsHandler,
	bootstrapRoundIndex uint64,
) (*Node, error) {
	var err error
	var consensusGroupSize uint32
	consensusGroupSize, err = getConsensusGroupSize(coreComponents.GenesisNodesSetup(), processComponents.ShardCoordinator())
	if err != nil {
		return nil, err
	}

	var txAccumulator core.Accumulator
	txAccumulatorConfig := config.Antiflood.TxAccumulator
	txAccumulator, err = accumulator.NewTimeAccumulator(
		time.Duration(txAccumulatorConfig.MaxAllowedTimeInMilliseconds)*time.Millisecond,
		time.Duration(txAccumulatorConfig.MaxDeviationTimeInMilliseconds)*time.Millisecond,
	)
	if err != nil {
		return nil, err
	}

	prepareOpenTopics(networkComponents.InputAntiFloodHandler(), processComponents.ShardCoordinator())

	peerDenialEvaluator, err := blackList.NewPeerDenialEvaluator(
		networkComponents.PeerBlackListHandler(),
		networkComponents.PubKeyCacher(),
		processComponents.PeerShardMapper(),
	)
	if err != nil {
		return nil, err
	}

	err = networkComponents.NetworkMessenger().SetPeerDenialEvaluator(peerDenialEvaluator)
	if err != nil {
		return nil, err
	}

	genesisTime := time.Unix(coreComponents.GenesisNodesSetup().GetStartTime(), 0)

	var nd *Node
	nd, err = NewNode(
		WithCoreComponents(coreComponents),
		WithCryptoComponents(cryptoComponents),
		WithNetworkComponents(networkComponents),
		WithBootstrapComponents(bootstrapComponents),
		WithStateComponents(stateComponents),
		WithDataComponents(dataComponents),
		WithStatusComponents(statusComponents),
		WithProcessComponents(processComponents),
		WithHeartbeatComponents(heartbeatComponents),
		WithConsensusComponents(consensusComponents),
		WithInitialNodesPubKeys(coreComponents.GenesisNodesSetup().InitialNodesPubKeys()),
		WithRoundDuration(coreComponents.GenesisNodesSetup().GetRoundDuration()),
		WithConsensusGroupSize(int(consensusGroupSize)),
		WithGenesisTime(genesisTime),
		WithConsensusType(config.Consensus.Type),
		WithBootstrapRoundIndex(bootstrapRoundIndex),
		WithPeerDenialEvaluator(peerDenialEvaluator),
		WithRequestedItemsHandler(processComponents.RequestedItemsHandler()),
		WithTxAccumulator(txAccumulator),
		WithHardforkTrigger(consensusComponents.HardforkTrigger()),
		WithSignatureSize(config.ValidatorPubkeyConverter.SignatureLength),
		WithPublicKeySize(config.ValidatorPubkeyConverter.Length),
		WithNodeStopChannel(coreComponents.ChanStopNodeProcess()),
	)
	if err != nil {
		return nil, errors.New("error creating node: " + err.Error())
	}

	if processComponents.ShardCoordinator().SelfId() < processComponents.ShardCoordinator().NumberOfShards() {
		err = nd.CreateShardedStores()
		if err != nil {
			return nil, err
		}
	}

	err = nodeDebugFactory.CreateInterceptedDebugHandler(
		nd,
		processComponents.InterceptorsContainer(),
		processComponents.ResolversFinder(),
		config.Debug.InterceptorResolver,
	)
	if err != nil {
		return nil, err
	}

	return nd, nil
}
