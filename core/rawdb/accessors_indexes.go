// Copyright 2018 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package rawdb

import (
	"github.com/Onther-Tech/plasma-evm/common"
	"github.com/Onther-Tech/plasma-evm/core/types"
	"github.com/Onther-Tech/plasma-evm/log"
	"github.com/Onther-Tech/plasma-evm/rlp"
)

// ReadTxLookupEntry retrieves the positional metadata associated with a transaction
// hash to allow retrieving the transaction or receipt by hash.
func ReadTxLookupEntry(db DatabaseReader, hash common.Hash) (common.Hash, uint64, uint64) {
	data, _ := db.Get(txLookupKey(hash))
	if len(data) == 0 {
		return common.Hash{}, 0, 0
	}
	var entry TxLookupEntry
	if err := rlp.DecodeBytes(data, &entry); err != nil {
		log.Error("Invalid transaction lookup entry RLP", "hash", hash, "err", err)
		return common.Hash{}, 0, 0
	}
	return entry.BlockHash, entry.BlockIndex, entry.Index
}

// WriteTxLookupEntries stores a positional metadata for every transaction from
// a block, enabling hash based transaction and receipt lookups.
func WriteTxLookupEntries(db DatabaseWriter, block *types.Block) {
	for i, tx := range block.Transactions() {
		entry := TxLookupEntry{
			BlockHash:  block.Hash(),
			BlockIndex: block.NumberU64(),
			Index:      uint64(i),
		}
		data, err := rlp.EncodeToBytes(entry)
		if err != nil {
			log.Crit("Failed to encode transaction lookup entry", "err", err)
		}
		if err := db.Put(txLookupKey(tx.Hash()), data); err != nil {
			log.Crit("Failed to store transaction lookup entry", "err", err)
		}
	}
}

// DeleteTxLookupEntry removes all transaction data associated with a hash.
func DeleteTxLookupEntry(db DatabaseDeleter, hash common.Hash) {
	db.Delete(txLookupKey(hash))
}

// ReadInvalidExitReceiptsLookupEntry retrieves the metadata associated with invalid exit receipts.
func ReadInvalidExitReceiptsLookupEntry(db DatabaseReader, hash common.Hash, num uint64, fork uint64) (common.Hash, uint64, []uint64) {
	data, _ := db.Get(invalidExitReceiptsLookupKey(fork, num, hash))
	if len(data) == 0 {
		return common.Hash{}, 0, nil
	}

	var entry InvalidExitReceiptsLookupEntry
	if err := rlp.DecodeBytes(data, &entry); err != nil {
		log.Error("Invalid invalid exit receipt lookup entry RLP", "hash", hash, "err", err)
		return common.Hash{}, 0, nil
	}
	return entry.BlockHash, entry.BlockIndex, entry.Indices
}

// WriteInvalidExitReceiptsLookupEntry stores a metadata for invalid exit receipts.
func WriteInvalidExitReceiptsLookupEntry(db DatabaseWriter, hash common.Hash, num uint64, fork uint64, indices []uint64) {
	entry := InvalidExitReceiptsLookupEntry{
		BlockHash:  hash,
		BlockIndex: num,
		Indices:    indices,
	}
	data, err := rlp.EncodeToBytes(entry)
	if err != nil {
		log.Crit("Failed to encode invalid exit receipt lookup entries", "err", err)
	}
	if err := db.Put(invalidExitReceiptsLookupKey(fork, num, hash), data); err != nil {
		log.Crit("Failed to store transaction lookup entry", "err", err)
	}
}

// DeleteInvalidExitReceiptsLookupEntry removes matadata for invalid exit receipts.
func DeleteInvalidExitReceiptsLookupEntry(db DatabaseDeleter, hash common.Hash, num uint64, fork uint64) {
	db.Delete(invalidExitReceiptsLookupKey(fork, num, hash))
}

// ReadInvalidExitReceipts retrieves all the invalid exit receipts.
func ReadInvalidExitReceipts(db DatabaseReader, hash common.Hash, num uint64, fork uint64) map[uint64]*types.Receipt {
	blockHash, blockNumber, indices := ReadInvalidExitReceiptsLookupEntry(db, hash, num, fork)
	if blockHash == (common.Hash{}) {
		return nil
	}
	receipts := ReadReceipts(db, blockHash, blockNumber)
	invalidExitReceipts := make(map[uint64]*types.Receipt)

	if len(receipts) == 0 {
		return nil
	}
	for _, index := range indices {
		invalidExitReceipts[index] = receipts[index]
	}
	return invalidExitReceipts
}

// ReadTransaction retrieves a specific transaction from the database, along with
// its added positional metadata.
func ReadTransaction(db DatabaseReader, hash common.Hash) (*types.Transaction, common.Hash, uint64, uint64) {
	blockHash, blockNumber, txIndex := ReadTxLookupEntry(db, hash)
	if blockHash == (common.Hash{}) {
		return nil, common.Hash{}, 0, 0
	}
	body := ReadBody(db, blockHash, blockNumber)
	if body == nil || len(body.Transactions) <= int(txIndex) {
		log.Error("Transaction referenced missing", "number", blockNumber, "hash", blockHash, "index", txIndex)
		return nil, common.Hash{}, 0, 0
	}
	return body.Transactions[txIndex], blockHash, blockNumber, txIndex
}

// ReadReceipt retrieves a specific transaction receipt from the database, along with
// its added positional metadata.
func ReadReceipt(db DatabaseReader, hash common.Hash) (*types.Receipt, common.Hash, uint64, uint64) {
	blockHash, blockNumber, receiptIndex := ReadTxLookupEntry(db, hash)
	if blockHash == (common.Hash{}) {
		return nil, common.Hash{}, 0, 0
	}
	receipts := ReadReceipts(db, blockHash, blockNumber)
	if len(receipts) <= int(receiptIndex) {
		log.Error("Receipt refereced missing", "number", blockNumber, "hash", blockHash, "index", receiptIndex)
		return nil, common.Hash{}, 0, 0
	}
	return receipts[receiptIndex], blockHash, blockNumber, receiptIndex
}

// ReadBloomBits retrieves the compressed bloom bit vector belonging to the given
// section and bit index from the.
func ReadBloomBits(db DatabaseReader, bit uint, section uint64, head common.Hash) ([]byte, error) {
	return db.Get(bloomBitsKey(bit, section, head))
}

// WriteBloomBits stores the compressed bloom bits vector belonging to the given
// section and bit index.
func WriteBloomBits(db DatabaseWriter, bit uint, section uint64, head common.Hash, bits []byte) {
	if err := db.Put(bloomBitsKey(bit, section, head), bits); err != nil {
		log.Crit("Failed to store bloom bits", "err", err)
	}
}
