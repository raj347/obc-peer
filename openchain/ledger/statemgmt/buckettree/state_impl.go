/*
Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements.  See the NOTICE file
distributed with this work for additional information
regarding copyright ownership.  The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License.  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied.  See the License for the
specific language governing permissions and limitations
under the License.
*/

package buckettree

import (
	"bytes"

	"github.com/op/go-logging"
	"github.com/openblockchain/obc-peer/openchain/db"
	"github.com/openblockchain/obc-peer/openchain/ledger/statemgmt"
	"github.com/tecbot/gorocksdb"
)

var logger = logging.MustGetLogger("buckettree")

// StateImpl - implements the interface - 'statemgmt.HashableState'
type StateImpl struct {
	dataNodesDelta         *dataNodesDelta
	bucketTreeDelta        *bucketTreeDelta
	persistedStateHash     []byte
	lastComputedCryptoHash []byte
	recomputeCryptoHash    bool
}

// NewStateImpl constructs a new StateImpl
func NewStateImpl() *StateImpl {
	return &StateImpl{}
}

// Initialize - method implementation for interface 'statemgmt.HashableState'
func (stateImpl *StateImpl) Initialize(configs map[string]interface{}) error {
	initConfig(configs)
	rootBucketNode, err := fetchBucketNodeFromDB(constructRootBucketKey())
	if err != nil {
		return err
	}
	if rootBucketNode != nil {
		stateImpl.persistedStateHash = rootBucketNode.computeCryptoHash()
		stateImpl.lastComputedCryptoHash = stateImpl.persistedStateHash
	}
	return nil

	// We can create a cache and keep all the bucket nodes pre-loaded.
	// Since, the bucket nodes do not contain actual data and max possible
	// buckets are pre-determined, the memory demand may not be very high or can easily
	// be controlled - by keeping seletive buckets in the cache (most likely first few levels of the bucket tree - because,
	// higher the level of the bucket, more are the chances that the bucket would be required for recomputation of hash)
}

// Get - method implementation for interface 'statemgmt.HashableState'
func (stateImpl *StateImpl) Get(chaincodeID string, key string) ([]byte, error) {
	dataKey := newDataKey(chaincodeID, key)
	dataNode, err := fetchDataNodeFromDB(dataKey)
	if err != nil {
		return nil, err
	}
	if dataNode == nil {
		return nil, nil
	}
	return dataNode.value, nil
}

// PrepareWorkingSet - method implementation for interface 'statemgmt.HashableState'
func (stateImpl *StateImpl) PrepareWorkingSet(stateDelta *statemgmt.StateDelta) error {
	logger.Debug("Enter - PrepareWorkingSet()")
	if stateDelta.IsEmpty() {
		logger.Debug("Ignoring working-set as it is empty")
		return nil
	}
	stateImpl.dataNodesDelta = newDataNodesDelta(stateDelta)
	stateImpl.bucketTreeDelta = newBucketTreeDelta()
	stateImpl.recomputeCryptoHash = true
	return nil
}

// ClearWorkingSet - method implementation for interface 'statemgmt.HashableState'
func (stateImpl *StateImpl) ClearWorkingSet(changesPersisted bool) {
	logger.Debug("Enter - ClearWorkingSet()")
	stateImpl.dataNodesDelta = nil
	stateImpl.bucketTreeDelta = nil
	stateImpl.recomputeCryptoHash = false
	if changesPersisted {
		stateImpl.persistedStateHash = stateImpl.lastComputedCryptoHash
	} else {
		stateImpl.lastComputedCryptoHash = stateImpl.persistedStateHash
	}
}

// ComputeCryptoHash - method implementation for interface 'statemgmt.HashableState'
func (stateImpl *StateImpl) ComputeCryptoHash() ([]byte, error) {
	logger.Debug("Enter - ComputeCryptoHash()")
	if stateImpl.recomputeCryptoHash {
		logger.Debug("Recomputing crypto-hash...")
		err := stateImpl.processDataNodeDelta()
		if err != nil {
			return nil, err
		}
		err = stateImpl.processBucketTreeDelta()
		if err != nil {
			return nil, err
		}
		stateImpl.lastComputedCryptoHash = stateImpl.computeRootNodeCryptoHash()
		stateImpl.recomputeCryptoHash = false
	} else {
		logger.Debug("Returing existing crypto-hash as recomputation not required")
	}
	return stateImpl.lastComputedCryptoHash, nil
}

func (stateImpl *StateImpl) processDataNodeDelta() error {
	afftectedBuckets := stateImpl.dataNodesDelta.getAffectedBuckets()
	for _, bucketKey := range afftectedBuckets {
		updatedDataNodes := stateImpl.dataNodesDelta.getSortedDataNodesFor(bucketKey)
		existingDataNodes, err := fetchDataNodesFromDBFor(bucketKey)
		if err != nil {
			return err
		}
		cryptoHashForBucket := computeDataNodesCryptoHash(bucketKey, updatedDataNodes, existingDataNodes)
		logger.Debug("Crypto-hash for lowest-level bucket [%s] is [%x]", bucketKey, cryptoHashForBucket)
		parentBucket := stateImpl.bucketTreeDelta.getOrCreateBucketNode(bucketKey.getParentKey())
		parentBucket.setChildCryptoHash(bucketKey, cryptoHashForBucket)
	}
	return nil
}

func (stateImpl *StateImpl) processBucketTreeDelta() error {
	secondLastLevel := conf.getLowestLevel() - 1
	for level := secondLastLevel; level >= 0; level-- {
		bucketNodes := stateImpl.bucketTreeDelta.getBucketNodesAt(level)
		for _, bucketNode := range bucketNodes {
			logger.Debug("bucketNode in tree-delta [%s]", bucketNode)
			dbBucketNode, err := fetchBucketNodeFromDB(bucketNode.bucketKey)
			logger.Debug("bucket node from db [%s]", dbBucketNode)
			if err != nil {
				return err
			}
			if dbBucketNode != nil {
				bucketNode.mergeBucketNode(dbBucketNode)
				logger.Debug("After merge... bucketNode in tree-delta [%s]", bucketNode)
			}
			if level == 0 {
				return nil
			}
			logger.Debug("Computing cryptoHash for bucket [%s]", bucketNode)
			cryptoHash := bucketNode.computeCryptoHash()
			logger.Debug("cryptoHash for bucket [%s] is [%x]", bucketNode, cryptoHash)
			parentBucket := stateImpl.bucketTreeDelta.getOrCreateBucketNode(bucketNode.bucketKey.getParentKey())
			parentBucket.setChildCryptoHash(bucketNode.bucketKey, cryptoHash)
		}
	}
	return nil
}

func (stateImpl *StateImpl) computeRootNodeCryptoHash() []byte {
	return stateImpl.bucketTreeDelta.getRootNode().computeCryptoHash()
}

func computeDataNodesCryptoHash(bucketKey *bucketKey, updatedNodes dataNodes, existingNodes dataNodes) []byte {
	logger.Debug("Computing crypto-hash for bucket [%s]. numUpdatedNodes=[%d], numExistingNodes=[%d]", bucketKey, len(updatedNodes), len(existingNodes))
	bucketHashCalculator := newBucketHashCalculator(bucketKey)
	i := 0
	j := 0
	for i < len(updatedNodes) && j < len(existingNodes) {
		updatedNode := updatedNodes[i]
		existingNode := existingNodes[j]
		c := bytes.Compare(updatedNode.dataKey.compositeKey, existingNode.dataKey.compositeKey)
		var nextNode *dataNode
		switch c {
		case -1:
			nextNode = updatedNode
			i++
		case 0:
			nextNode = updatedNode
			i++
			j++
		case 1:
			nextNode = existingNode
			j++
		}
		if !nextNode.isDelete() {
			bucketHashCalculator.addNextNode(nextNode)
		}
	}

	var remainingNodes dataNodes
	if i < len(updatedNodes) {
		remainingNodes = updatedNodes[i:]
	} else if j < len(existingNodes) {
		remainingNodes = existingNodes[j:]
	}

	for _, remainingNode := range remainingNodes {
		if !remainingNode.isDelete() {
			bucketHashCalculator.addNextNode(remainingNode)
		}
	}
	return bucketHashCalculator.computeCryptoHash()
}

// AddChangesForPersistence - method implementation for interface 'statemgmt.HashableState'
func (stateImpl *StateImpl) AddChangesForPersistence(writeBatch *gorocksdb.WriteBatch) error {

	if stateImpl.dataNodesDelta == nil {
		return nil
	}

	if stateImpl.recomputeCryptoHash {
		_, err := stateImpl.ComputeCryptoHash()
		if err != nil {
			return nil
		}
	}
	stateImpl.addDataNodeChangesForPersistence(writeBatch)
	stateImpl.addBucketNodeChangesForPersistence(writeBatch)
	return nil
}

func (stateImpl *StateImpl) addDataNodeChangesForPersistence(writeBatch *gorocksdb.WriteBatch) {
	openchainDB := db.GetDBHandle()
	affectedBuckets := stateImpl.dataNodesDelta.getAffectedBuckets()
	for _, affectedBucket := range affectedBuckets {
		dataNodes := stateImpl.dataNodesDelta.getSortedDataNodesFor(affectedBucket)
		for _, dataNode := range dataNodes {
			if dataNode.isDelete() {
				writeBatch.DeleteCF(openchainDB.StateCF, dataNode.dataKey.getEncodedBytes())
			} else {
				writeBatch.PutCF(openchainDB.StateCF, dataNode.dataKey.getEncodedBytes(), dataNode.value)
			}
		}
	}
}

func (stateImpl *StateImpl) addBucketNodeChangesForPersistence(writeBatch *gorocksdb.WriteBatch) {
	openchainDB := db.GetDBHandle()
	secondLastLevel := conf.getLowestLevel() - 1
	for level := secondLastLevel; level >= 0; level-- {
		bucketNodes := stateImpl.bucketTreeDelta.getBucketNodesAt(level)
		for _, bucketNode := range bucketNodes {
			if bucketNode.markedForDeletion {
				writeBatch.DeleteCF(openchainDB.StateCF, bucketNode.bucketKey.getEncodedBytes())
			} else {
				writeBatch.PutCF(openchainDB.StateCF, bucketNode.bucketKey.getEncodedBytes(), bucketNode.marshal())
			}
			writeBatch.PutCF(openchainDB.StateCF, bucketNode.bucketKey.getEncodedBytes(), bucketNode.marshal())
		}
	}
}

// PerfHintKeyChanged - method implementation for interface 'statemgmt.HashableState'
func (stateImpl *StateImpl) PerfHintKeyChanged(chaincodeID string, key string) {
	// We can create a cache. Pull all the keys for the bucket (to which given key belongs) in a separate thread
	// This prefetching can help making method 'ComputeCryptoHash' faster.
}

// GetStateSnapshotIterator - method implementation for interface 'statemgmt.HashableState'
func (stateImpl *StateImpl) GetStateSnapshotIterator(snapshot *gorocksdb.Snapshot) (statemgmt.StateSnapshotIterator, error) {
	return newStateSnapshotIterator(snapshot)
}

// GetRangeScanIterator - method implementation for interface 'statemgmt.HashableState'
func (stateImpl *StateImpl) GetRangeScanIterator(chaincodeID string, startKey string, endKey string) (statemgmt.RangeScanIterator, error) {
	return newRangeScanIterator(chaincodeID, startKey, endKey)
}
