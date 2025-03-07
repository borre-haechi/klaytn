// Copyright 2019 The klaytn Authors
// This file is part of the klaytn library.
//
// The klaytn library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The klaytn library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the klaytn library. If not, see <http://www.gnu.org/licenses/>.

package reward

import (
	"errors"
	"sync"

	"github.com/klaytn/klaytn/blockchain"
	"github.com/klaytn/klaytn/blockchain/state"
	"github.com/klaytn/klaytn/blockchain/types"
	"github.com/klaytn/klaytn/common"
	"github.com/klaytn/klaytn/event"
	"github.com/klaytn/klaytn/params"
)

const (
	chainHeadChanSize = 100
)

// blockChain is an interface for blockchain.Blockchain used in reward package.
type blockChain interface {
	SubscribeChainHeadEvent(ch chan<- blockchain.ChainHeadEvent) event.Subscription
	GetBlockByNumber(number uint64) *types.Block
	StateAt(root common.Hash) (*state.StateDB, error)
	Config() *params.ChainConfig

	blockchain.ChainContext
}

type StakingManager struct {
	addressBookConnector *addressBookConnector
	stakingInfoCache     *stakingInfoCache
	stakingInfoDB        stakingInfoDB
	governanceHelper     governanceHelper
	blockchain           blockChain
	chainHeadChan        chan blockchain.ChainHeadEvent
	chainHeadSub         event.Subscription
}

var (
	// variables for sole StakingManager
	once           sync.Once
	stakingManager *StakingManager

	// errors for staking manager
	ErrStakingManagerNotSet = errors.New("staking manager is not set")
	ErrChainHeadChanNotSet  = errors.New("chain head channel is not set")
)

// NewStakingManager creates and returns StakingManager.
//
// On the first call, a StakingManager is created with given parameters.
// From next calls, the existing StakingManager is returned. (Parameters
// from the next calls will not affect.)
func NewStakingManager(bc blockChain, gh governanceHelper, db stakingInfoDB) *StakingManager {
	if bc != nil && gh != nil {
		// this is only called once
		once.Do(func() {
			stakingManager = &StakingManager{
				addressBookConnector: newAddressBookConnector(bc, gh),
				stakingInfoCache:     newStakingInfoCache(),
				stakingInfoDB:        db,
				governanceHelper:     gh,
				blockchain:           bc,
				chainHeadChan:        make(chan blockchain.ChainHeadEvent, chainHeadChanSize),
			}

			// Before migration, staking information of current and before should be stored in DB.
			//
			// Staking information from block of StakingUpdateInterval ahead is needed to create a block.
			// If there is no staking info in either cache, db or state trie, the node cannot make a block.
			// The information in state trie is deleted after state trie migration.
			blockchain.RegisterMigrationPrerequisites(func(blockNum uint64) error {
				if err := CheckStakingInfoStored(blockNum); err != nil {
					return err
				}
				return CheckStakingInfoStored(blockNum + params.StakingUpdateInterval())
			})
		})
	} else {
		logger.Error("unable to set StakingManager", "blockchain", bc, "governanceHelper", gh)
	}

	return stakingManager
}

func GetStakingManager() *StakingManager {
	return stakingManager
}

// GetStakingInfo returns a stakingInfo on the staking block of the given block number.
// Note that staking block is the block on which the associated staking information is stored and used during an interval.
func GetStakingInfo(blockNum uint64) *StakingInfo {
	stakingBlockNumber := params.CalcStakingBlockNumber(blockNum)
	logger.Debug("Staking information is requested", "blockNum", blockNum, "staking block number", stakingBlockNumber)
	return GetStakingInfoOnStakingBlock(stakingBlockNumber)
}

// GetStakingInfoOnStakingBlock returns a corresponding StakingInfo for a staking block number.
// If the given number is not on the staking block, it returns nil.
//
// Fixup for Gini coefficients:
// Klaytn core stores Gini: -1 in its database.
// We ensure GetStakingInfoOnStakingBlock() to always return meaningful Gini.
//   If cache hit                               -> fillMissingGini -> modifies cached in-memory object
//   If db hit                                  -> fillMissingGini -> write to cache
//   If read contract -> write to db (gini: -1) -> fillMissingGini -> write to cache
func GetStakingInfoOnStakingBlock(stakingBlockNumber uint64) *StakingInfo {
	if stakingManager == nil {
		logger.Error("unable to GetStakingInfo", "err", ErrStakingManagerNotSet)
		return nil
	}

	// shortcut if given block is not on staking update interval
	if !params.IsStakingUpdateInterval(stakingBlockNumber) {
		return nil
	}

	// Get staking info from cache
	if cachedStakingInfo := stakingManager.stakingInfoCache.get(stakingBlockNumber); cachedStakingInfo != nil {
		logger.Debug("StakingInfoCache hit.", "staking block number", stakingBlockNumber, "stakingInfo", cachedStakingInfo)
		// Fill in Gini coeff if not set. Modifies the cached object.
		if err := fillMissingGiniCoefficient(cachedStakingInfo, stakingBlockNumber); err != nil {
			logger.Warn("Cannot fill in gini coefficient", "staking block number", stakingBlockNumber, "err", err)
		}
		return cachedStakingInfo
	}

	// Get staking info from DB
	if storedStakingInfo, err := getStakingInfoFromDB(stakingBlockNumber); storedStakingInfo != nil && err == nil {
		logger.Debug("StakingInfoDB hit.", "staking block number", stakingBlockNumber, "stakingInfo", storedStakingInfo)
		// Fill in Gini coeff before adding to cache.
		if err := fillMissingGiniCoefficient(storedStakingInfo, stakingBlockNumber); err != nil {
			logger.Warn("Cannot fill in gini coefficient", "staking block number", stakingBlockNumber, "err", err)
		}
		stakingManager.stakingInfoCache.add(storedStakingInfo)
		return storedStakingInfo
	} else {
		logger.Debug("failed to get stakingInfo from DB", "err", err, "staking block number", stakingBlockNumber)
	}

	// Calculate staking info from block header and updates it to cache and db
	calcStakingInfo, err := updateStakingInfo(stakingBlockNumber)
	if calcStakingInfo == nil {
		logger.Error("failed to update stakingInfo", "staking block number", stakingBlockNumber, "err", err)
		return nil
	}

	logger.Debug("Get stakingInfo from header.", "staking block number", stakingBlockNumber, "stakingInfo", calcStakingInfo)
	return calcStakingInfo
}

// updateStakingInfo updates staking info in cache and db created from given block number.
func updateStakingInfo(blockNum uint64) (*StakingInfo, error) {
	if stakingManager == nil {
		return nil, ErrStakingManagerNotSet
	}

	stakingInfo, err := stakingManager.addressBookConnector.getStakingInfoFromAddressBook(blockNum)
	if err != nil {
		return nil, err
	}

	// Add to DB before setting Gini; DB will contain {Gini: -1}
	if err := AddStakingInfoToDB(stakingInfo); err != nil {
		logger.Debug("failed to write staking info to db", "err", err, "stakingInfo", stakingInfo)
		return stakingInfo, err
	}

	// Fill in Gini coeff before adding to cache
	if err := fillMissingGiniCoefficient(stakingInfo, blockNum); err != nil {
		logger.Warn("Cannot fill in gini coefficient", "blockNum", blockNum, "err", err)
	}

	// Add to cache after setting Gini
	stakingManager.stakingInfoCache.add(stakingInfo)

	logger.Info("Add a new stakingInfo to stakingInfoCache and stakingInfoDB", "blockNum", blockNum)
	logger.Debug("Added stakingInfo", "stakingInfo", stakingInfo)
	return stakingInfo, nil
}

// CheckStakingInfoStored makes sure the given staking info is stored in cache and DB
func CheckStakingInfoStored(blockNum uint64) error {
	if stakingManager == nil {
		return ErrStakingManagerNotSet
	}

	stakingBlockNumber := params.CalcStakingBlockNumber(blockNum)

	// skip checking if staking info is stored in DB
	if _, err := getStakingInfoFromDB(stakingBlockNumber); err == nil {
		return nil
	}

	// update staking info in DB and cache from address book
	_, err := updateStakingInfo(stakingBlockNumber)
	return err
}

// Fill in StakingInfo.Gini value if not set.
func fillMissingGiniCoefficient(stakingInfo *StakingInfo, number uint64) error {
	if !stakingInfo.UseGini {
		return nil
	}
	if stakingInfo.Gini >= 0 {
		return nil
	}

	// We reach here if UseGini == true && Gini == -1. There are two such cases.
	// - Gini was never been calculated, so it is DefaultGiniCoefficient.
	// - Gini was calculated but there was no eligible node, so Gini = -1.
	// For the second case, in theory we won't have to recalculalte Gini,
	// but there is no way to distinguish both. So we just recalculate.
	minStaking, err := stakingManager.governanceHelper.GetMinimumStakingAtNumber(number)
	if err != nil {
		return err
	}

	c := stakingInfo.GetConsolidatedStakingInfo()
	if c == nil {
		return errors.New("Cannot create ConsolidatedStakingInfo")
	}

	stakingInfo.Gini = c.CalcGiniCoefficientMinStake(minStaking)
	logger.Debug("Calculated missing Gini for stored StakingInfo", "number", number, "gini", stakingInfo.Gini)
	return nil
}

// StakingManagerSubscribe setups a channel to listen chain head event and starts a goroutine to update staking cache.
func StakingManagerSubscribe() {
	if stakingManager == nil {
		logger.Warn("unable to subscribe; this can slow down node", "err", ErrStakingManagerNotSet)
		return
	}

	stakingManager.chainHeadSub = stakingManager.blockchain.SubscribeChainHeadEvent(stakingManager.chainHeadChan)

	go handleChainHeadEvent()
}

func handleChainHeadEvent() {
	if stakingManager == nil {
		logger.Warn("unable to start chain head event", "err", ErrStakingManagerNotSet)
		return
	} else if stakingManager.chainHeadSub == nil {
		logger.Info("unable to start chain head event", "err", ErrChainHeadChanNotSet)
		return
	}

	defer StakingManagerUnsubscribe()

	logger.Info("Start listening chain head event to update stakingInfoCache.")

	for {
		// A real event arrived, process interesting content
		select {
		// Handle ChainHeadEvent
		case ev := <-stakingManager.chainHeadChan:
			if stakingManager.governanceHelper.ProposerPolicy() == params.WeightedRandom {
				// check and update if staking info is not valid before for the next update interval blocks
				stakingInfo := GetStakingInfo(ev.Block.NumberU64() + params.StakingUpdateInterval())
				if stakingInfo == nil {
					logger.Error("unable to fetch staking info", "blockNum", ev.Block.NumberU64())
				}
			}
		case <-stakingManager.chainHeadSub.Err():
			return
		}
	}
}

// StakingManagerUnsubscribe can unsubscribe a subscription on chain head event.
func StakingManagerUnsubscribe() {
	if stakingManager == nil {
		logger.Warn("unable to start chain head event", "err", ErrStakingManagerNotSet)
		return
	} else if stakingManager.chainHeadSub == nil {
		logger.Info("unable to start chain head event", "err", ErrChainHeadChanNotSet)
		return
	}

	stakingManager.chainHeadSub.Unsubscribe()
}

// TODO-Klaytn-Reward the following methods are used for testing purpose, it needs to be moved into test files.
// Unlike NewStakingManager(), SetTestStakingManager*() do not trigger once.Do().
// This way you can avoid irreversible side effects during tests.

// SetTestStakingManagerWithChain sets a full-featured staking manager with blockchain, database and cache.
// Note that this method is used only for testing purpose.
func SetTestStakingManagerWithChain(bc blockChain, gh governanceHelper, db stakingInfoDB) {
	SetTestStakingManager(&StakingManager{
		addressBookConnector: newAddressBookConnector(bc, gh),
		stakingInfoCache:     newStakingInfoCache(),
		stakingInfoDB:        db,
		governanceHelper:     gh,
		blockchain:           bc,
		chainHeadChan:        make(chan blockchain.ChainHeadEvent, chainHeadChanSize),
	})
}

// SetTestStakingManagerWithDB sets the staking manager with the given database.
// Note that this method is used only for testing purpose.
func SetTestStakingManagerWithDB(testDB stakingInfoDB) {
	SetTestStakingManager(&StakingManager{
		stakingInfoDB: testDB,
	})
}

// SetTestStakingManagerWithStakingInfoCache sets the staking manager with the given test staking information.
// Note that this method is used only for testing purpose.
func SetTestStakingManagerWithStakingInfoCache(testInfo *StakingInfo) {
	cache := newStakingInfoCache()
	cache.add(testInfo)
	SetTestStakingManager(&StakingManager{
		stakingInfoCache: cache,
	})
}

// SetTestStakingManager sets the staking manager for testing purpose.
// Note that this method is used only for testing purpose.
func SetTestStakingManager(sm *StakingManager) {
	stakingManager = sm
}
