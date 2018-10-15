// Copyright 2018 The dexon-consensus-core Authors
// This file is part of the dexon-consensus-core library.
//
// The dexon-consensus-core library is free software: you can redistribute it
// and/or modify it under the terms of the GNU Lesser General Public License as
// published by the Free Software Foundation, either version 3 of the License,
// or (at your option) any later version.
//
// The dexon-consensus-core library is distributed in the hope that it will be
// useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser
// General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the dexon-consensus-core library. If not, see
// <http://www.gnu.org/licenses/>.

package dex

import (
	"fmt"
	"time"

	dexCore "github.com/dexon-foundation/dexon-consensus-core/core"
	"github.com/dexon-foundation/dexon-consensus-core/core/blockdb"
	coreEcdsa "github.com/dexon-foundation/dexon-consensus-core/core/crypto/ecdsa"

	"github.com/dexon-foundation/dexon/accounts"
	"github.com/dexon-foundation/dexon/consensus"
	"github.com/dexon-foundation/dexon/consensus/dexcon"
	"github.com/dexon-foundation/dexon/core"
	"github.com/dexon-foundation/dexon/core/bloombits"
	"github.com/dexon-foundation/dexon/core/rawdb"
	"github.com/dexon-foundation/dexon/core/vm"
	"github.com/dexon-foundation/dexon/dex/gasprice"
	"github.com/dexon-foundation/dexon/eth/downloader"
	"github.com/dexon-foundation/dexon/ethdb"
	"github.com/dexon-foundation/dexon/event"
	"github.com/dexon-foundation/dexon/internal/ethapi"
	"github.com/dexon-foundation/dexon/log"
	"github.com/dexon-foundation/dexon/node"
	"github.com/dexon-foundation/dexon/p2p"
	"github.com/dexon-foundation/dexon/params"
	"github.com/dexon-foundation/dexon/rpc"
)

// Dexon implementes the DEXON fullnode service.
type Dexon struct {
	config      *Config
	chainConfig *params.ChainConfig

	// Channel for shutting down the service
	shutdownChan chan bool // Channel for shutting down the Ethereum

	// Handlers
	txPool          *core.TxPool
	blockchain      *core.BlockChain
	protocolManager *ProtocolManager

	// DB interfaces
	chainDb ethdb.Database // Block chain database

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager

	bloomRequests chan chan *bloombits.Retrieval // Channel receiving bloom data retrieval requests
	bloomIndexer  *core.ChainIndexer             // Bloom indexer operating during block imports

	APIBackend *DexAPIBackend

	// Dexon consensus.
	app        *DexconApp
	governance *DexconGovernance
	network    *DexconNetwork
	blockdb    blockdb.BlockDatabase
	consensus  *dexCore.Consensus

	networkID     uint64
	netRPCService *ethapi.PublicNetAPI
}

func New(ctx *node.ServiceContext, config *Config) (*Dexon, error) {
	// Consensus.
	db, err := blockdb.NewLevelDBBackedBlockDB("main.blockdb")
	if err != nil {
		panic(err)
	}

	chainDb, err := CreateDB(ctx, config, "chaindata")
	if err != nil {
		return nil, err
	}
	chainConfig, genesisHash, genesisErr := core.SetupGenesisBlock(chainDb,
		config.Genesis)
	if _, ok := genesisErr.(*params.ConfigCompatError); genesisErr != nil && !ok {
		return nil, genesisErr
	}
	log.Info("Initialised chain configuration", "config", chainConfig)

	if !config.SkipBcVersionCheck {
		bcVersion := rawdb.ReadDatabaseVersion(chainDb)
		if bcVersion != core.BlockChainVersion && bcVersion != 0 {
			return nil, fmt.Errorf("Blockchain DB version mismatch (%d / %d).\n",
				bcVersion, core.BlockChainVersion)
		}
		rawdb.WriteDatabaseVersion(chainDb, core.BlockChainVersion)
	}
	dex := &Dexon{
		config:         config,
		chainDb:        chainDb,
		chainConfig:    chainConfig,
		eventMux:       ctx.EventMux,
		accountManager: ctx.AccountManager,
		shutdownChan:   make(chan bool),
		networkID:      config.NetworkId,
		bloomRequests:  make(chan chan *bloombits.Retrieval),
		bloomIndexer:   NewBloomIndexer(chainDb, params.BloomBitsBlocks, params.BloomConfirms),
		blockdb:        db,
		engine:         dexcon.New(&params.DexconConfig{}),
	}

	var (
		vmConfig = vm.Config{
			EnablePreimageRecording: config.EnablePreimageRecording,
			EWASMInterpreter:        config.EWASMInterpreter,
			EVMInterpreter:          config.EVMInterpreter,
		}
		cacheConfig = &core.CacheConfig{Disabled: config.NoPruning, TrieCleanLimit: config.TrieCleanCache, TrieDirtyLimit: config.TrieDirtyCache, TrieTimeLimit: config.TrieTimeout}
	)
	dex.blockchain, err = core.NewBlockChain(chainDb, cacheConfig, dex.chainConfig, dex.engine, vmConfig, nil)

	// Rewind the chain in case of an incompatible config upgrade.
	if compat, ok := genesisErr.(*params.ConfigCompatError); ok {
		log.Warn("Rewinding chain to upgrade configuration", "err", compat)
		dex.blockchain.SetHead(compat.RewindTo)
		rawdb.WriteChainConfig(chainDb, genesisHash, chainConfig)
	}
	dex.bloomIndexer.Start(dex.blockchain)

	if config.TxPool.Journal != "" {
		config.TxPool.Journal = ctx.ResolvePath(config.TxPool.Journal)
	}
	dex.txPool = core.NewTxPool(config.TxPool, dex.chainConfig, dex.blockchain)

	dex.APIBackend = &DexAPIBackend{dex, nil}
	gpoParams := config.GPO
	if gpoParams.Default == nil {
		gpoParams.Default = config.DefaultGasPrice
	}
	dex.APIBackend.gpo = gasprice.NewOracle(dex.APIBackend, gpoParams)

	dex.governance = NewDexconGovernance(dex.APIBackend, dex.chainConfig, config.PrivateKey)
	dex.app = NewDexconApp(dex.txPool, dex.blockchain, dex.governance, chainDb, config, vmConfig)

	pm, err := NewProtocolManager(dex.chainConfig, config.SyncMode,
		config.NetworkId, dex.eventMux, dex.txPool, dex.engine, dex.blockchain,
		chainDb, dex.governance)
	if err != nil {
		return nil, err
	}

	dex.protocolManager = pm
	dex.network = NewDexconNetwork(pm)

	privKey := coreEcdsa.NewPrivateKeyFromECDSA(config.PrivateKey)
	dex.consensus = dexCore.NewConsensus(time.Now().Add(10*time.Second),
		dex.app, dex.governance, db, dex.network, privKey)
	return dex, nil
}

func (s *Dexon) Protocols() []p2p.Protocol {
	return nil
}

func (s *Dexon) APIs() []rpc.API {
	return nil
}

func (s *Dexon) Start(srvr *p2p.Server) error {
	// Start the bloom bits servicing goroutines
	s.startBloomHandlers(params.BloomBitsBlocks)

	// Start the RPC service
	s.netRPCService = ethapi.NewPublicNetAPI(srvr, s.NetVersion())

	// Figure out a max peers count based on the server limits
	maxPeers := srvr.MaxPeers
	if s.config.LightServ > 0 {
		if s.config.LightPeers >= srvr.MaxPeers {
			return fmt.Errorf("invalid peer config: light peer count (%d) >= total peer count (%d)", s.config.LightPeers, srvr.MaxPeers)
		}
		maxPeers -= s.config.LightPeers
	}
	// Start the networking layer and the light server if requested
	s.protocolManager.Start(srvr, maxPeers)

	return nil
}

func (s *Dexon) Stop() error {
	return nil
}

// CreateDB creates the chain database.
func CreateDB(ctx *node.ServiceContext, config *Config, name string) (ethdb.Database, error) {
	db, err := ctx.OpenDatabase(name, config.DatabaseCache, config.DatabaseHandles)
	if err != nil {
		return nil, err
	}
	if db, ok := db.(*ethdb.LDBDatabase); ok {
		db.Meter("eth/db/chaindata/")
	}
	return db, nil
}

func (d *Dexon) AccountManager() *accounts.Manager  { return d.accountManager }
func (d *Dexon) BlockChain() *core.BlockChain       { return d.blockchain }
func (d *Dexon) TxPool() *core.TxPool               { return d.txPool }
func (d *Dexon) DexVersion() int                    { return int(d.protocolManager.SubProtocols[0].Version) }
func (d *Dexon) EventMux() *event.TypeMux           { return d.eventMux }
func (d *Dexon) Engine() consensus.Engine           { return d.engine }
func (d *Dexon) ChainDb() ethdb.Database            { return d.chainDb }
func (d *Dexon) Downloader() *downloader.Downloader { return d.protocolManager.downloader }
func (d *Dexon) NetVersion() uint64                 { return d.networkID }
