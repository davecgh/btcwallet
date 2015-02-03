/*
 * Copyright (c) 2014 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package wtxmgr

import (
	"bytes"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwire"
)

const (
	// LatestTxStoreVersion is the most recent tx store version.
	LatestTxStoreVersion = 1
)

// maybeConvertDbError converts the passed error to a TxStoreError with an
// error code of ErrDatabase if it is not already a TxStoreError.  This is
// useful for potential errors returned from managed transaction an other parts
// of the walletdb database.
func maybeConvertDbError(err error) error {
	// When the error is already a TxStoreError, just return it.
	if _, ok := err.(TxStoreError); ok {
		return err
	}

	return txStoreError(ErrDatabase, err.Error(), err)
}

// Key names for various database fields.
var (
	// Bucket names.
	mainBucketName        = []byte("main")
	blocksBucketName      = []byte("blocks")
	unspentBucketName     = []byte("unspent")
	txRecordsBucketName   = []byte("txrecords")
	unconfirmedBucketName = []byte("unconfirmed")

	metaBucketName             = []byte("meta")
	numUnconfirmedRecordsName  = []byte("numunconfirmedrecords")
	numUnconfirmedSpendsName   = []byte("numunconfirmedspends")
	numConfirmedSpendsName     = []byte("numconfirmedspends")
	numSpentBlockOutPointsName = []byte("numconfirmedspends")
	numBlockTxRecordsName      = []byte("numtxrecords")
	numBlocksName              = []byte("numblocks")

	// blockTxIdx indexes transactions mined in a block and maps the block
	// height to the list of hashes of the transactions
	blockTxIdxBucketName = []byte("blocktxidx")
	// blockTxIdx indexes transactions mined in a block and maps the block
	// height to the list of block keys of the transactions
	blockTxKeyIdxBucketName = []byte("blocktxkeyidx")

	// Unconfirmed Store related key names (unconfirmed bucket)

	// spentBlockOutPointIdx maps from spent outputs from mined transaction to
	// the unconfirmed transaction which spends it.  An additional map is
	// included to lookup the output key by its outpoint.
	spentBlockOutPointIdxBucketName    = []byte("spentblockoutpointidx")
	spentBlockOutPointKeyIdxBucketName = []byte("spentblockoutpointkeyidx")

	// spentUnconfirmedIdx maps from an unconfirmed outpoint to the unconfirmed
	// transaction which spends it.
	spentUnconfirmedIdxBucketName = []byte("spentunconfirmedidx")

	// prevOutPointIdx maps all previous outputs to the transaction record of
	// the unconfirmed transaction which spends it.  This is primarly designed
	// to assist with double spend detection without iterating through each
	// value of the txs map.
	prevOutPointIdxBucketName = []byte("prevoutpointidx")

	// Db related key names (main bucket).
	txstoreVersionName    = []byte("txstorever")
	txstoreCreateDateName = []byte("txstorecreated")
)

// uint32ToBytes converts a 32 bit unsigned integer into a 4-byte slice in
// little-endian order: 1 -> [1 0 0 0].
func uint32ToBytes(number uint32) []byte {
	buf := make([]byte, 4)
	byteOrder.PutUint32(buf, number)
	return buf
}

// shaHashToBytes converts a btcwire.ShaHash into a 32-byte slice
func shaHashToBytes(hash *btcwire.ShaHash) []byte {
	buf := make([]byte, 32)
	copy(buf[0:32], hash[:])
	return buf
}

// outPointToBytes converts a btcwire.OutPoint into a 36-byte slice
// It is serialized by the hash followed by the uint32 index
func outPointToBytes(op *btcwire.OutPoint) []byte {
	buf := make([]byte, 36)
	copy(buf[0:32], op.Hash[:])
	byteOrder.PutUint32(buf[32:36], op.Index)
	return buf
}

// blockTxKeyToBytes converts a BlockTxKey into a 8-byte slice
// It is serialized as the uint32 block height followed by the
// uint32 block index
func blockTxKeyToBytes(b *BlockTxKey) []byte {
	buf := make([]byte, 8)
	byteOrder.PutUint32(buf[0:4], uint32(b.BlockHeight))
	byteOrder.PutUint32(buf[4:8], uint32(b.BlockIndex))
	return buf
}

// blockOutputKeyToBytes converts a BlockOutputKey into a 12-byte slice
// It is serialized as the uint32 block index followed by the
// uint32 block height and uint32 output index
func blockOutputKeyToBytes(key *BlockOutputKey) []byte {
	buf := make([]byte, 12)
	byteOrder.PutUint32(buf[0:4], uint32(key.BlockIndex))
	byteOrder.PutUint32(buf[4:8], uint32(key.BlockHeight))
	byteOrder.PutUint32(buf[8:12], uint32(key.OutputIndex))
	return buf
}

// deserializeOutPoint deserializes the passed serialized outpoint information.
func deserializeOutPoint(serializedOutPoint []byte) (*btcwire.OutPoint, error) {
	// The serialized outpoint format is:
	//   <hash><index>
	//
	//   32 bytes hash length + 4 bytes index
	if len(serializedOutPoint) < 36 {
		str := fmt.Sprintf("malformed serialized outpoint")
		return nil, txStoreError(ErrDatabase, str, nil)
	}
	row := new(btcwire.OutPoint)
	copy(row.Hash[:], serializedOutPoint[0:32])
	row.Index = byteOrder.Uint32(serializedOutPoint[32:36])
	return row, nil
}

// deserializeOutPoint deserializes the passed serialized block information.
func deserializeBlockRow(k []byte, serializedBlock []byte) (*Block, error) {
	// The serialized block format is:
	//   <blockhash><blocktime><blockheight><spendable><reward>
	//
	//   32 bytes hash length + 8 bytes timestamp + 4 bytes block height +
	//   8 bytes spendable amount + 8 bytes reward amount
	if len(serializedBlock) < 60 {
		str := fmt.Sprintf("malformed serialized block for key %x",
			k)
		return nil, txStoreError(ErrDatabase, str, nil)
	}

	row := new(Block)

	// Read block hash, unix time (int64), and height (int32).
	copy(row.Hash[:], serializedBlock[0:32])
	row.Time = time.Unix(int64(byteOrder.Uint64(serializedBlock[32:40])), 0)
	row.Height = int32(byteOrder.Uint32(serializedBlock[40:44]))

	// Read amount deltas as a result of transactions in this block.  This
	// is the net total spendable balance as a result of transaction debits
	// and credits, and the block reward (not immediately spendable) for
	// coinbase outputs.  Both are int64s.
	spendable := btcutil.Amount(int64(byteOrder.Uint64(serializedBlock[44:52])))
	reward := btcutil.Amount(int64(byteOrder.Uint64(serializedBlock[52:60])))
	row.amountDeltas.Spendable = spendable
	row.amountDeltas.Reward = reward
	return row, nil
}

// serializeBlockRow returns the serialization of the passed block row.
func serializeBlockRow(row *Block) []byte {
	// The serialized block format is:
	//   <blockhash><blocktime><blockheight><spendable><reward>
	//
	//   32 bytes hash length + 8 bytes timestamp + 4 bytes block height +
	//   8 bytes spendable amount + 8 bytes reward amount
	buf := make([]byte, 60)

	// Write block hash, unix time (int64), and height (int32).
	copy(buf[0:32], row.Hash[:])
	byteOrder.PutUint64(buf[32:40], uint64(row.Time.Unix()))
	byteOrder.PutUint32(buf[40:44], uint32(row.Height))

	// Write amount deltas as a result of transactions in this block.
	// This is the net total spendable balance as a result of transaction
	// debits and credits, and the block reward (not immediately spendable)
	// for coinbase outputs.  Both are int64s.
	byteOrder.PutUint64(buf[44:52], uint64(row.amountDeltas.Spendable))
	byteOrder.PutUint64(buf[52:60], uint64(row.amountDeltas.Reward))
	return buf
}

// updateTxRecord updates a tx record in the txrecords bucket
// It needs to be called when tx record details like credits,
// debits etc change
func updateTxRecord(tx walletdb.Tx, t *txRecord) error {
	bucket := tx.RootBucket().Bucket(txRecordsBucketName)

	// Write the serialized txrecord keyed by the tx hash.
	serializedRow, err := serializeTxRecordRow(t)
	if err != nil {
		str := fmt.Sprintf("failed to serialize txrecord '%s'", t.tx.Sha())
		return txStoreError(ErrDatabase, str, err)
	}
	err = bucket.Put(shaHashToBytes(t.tx.Sha()), serializedRow)
	if err != nil {
		str := fmt.Sprintf("failed to store txrecord '%s'", t.tx.Sha())
		return txStoreError(ErrDatabase, str, err)
	}
	return nil
}

// putTxRecord inserts a given tx record to the txrecords bucket
// It also updates the block tx indexes
// It needs to be called when a new tx record is inserted
func putTxRecord(tx walletdb.Tx, b *Block, t *txRecord) error {
	if err := updateTxRecord(tx, t); err != nil {
		str := fmt.Sprintf("failed to update txrecord '%s'", t.tx.Sha())
		return txStoreError(ErrDatabase, str, err)
	}
	n, err := fetchNumBlockTxRecords(tx, b.Height)
	if err != nil {
		return err
	}
	n++
	if err := putNumBlockTxRecords(tx, b.Height, n); err != nil {
		return err
	}
	return updateBlockTxIdx(tx, b, t.tx)
}

// putMeta inserts a metadata counter with the given key
// The counter is used for keeping track of number of entries in various buckets
// like blocks, tx records etc.
func putMeta(tx walletdb.Tx, key []byte, n int32) error {
	bucket := tx.RootBucket().Bucket(metaBucketName)
	err := bucket.Put(key, uint32ToBytes(uint32(n)))
	if err != nil {
		str := fmt.Sprintf("failed to store meta key '%s'", key)
		return txStoreError(ErrDatabase, str, err)
	}
	return nil
}

// fetchMeta fetches the metadata counter with the given key
func fetchMeta(tx walletdb.Tx, key []byte) (int32, error) {
	bucket := tx.RootBucket().Bucket(metaBucketName)

	val := bucket.Get(key)
	// Return 0 if the metadata is uninitialized
	if val == nil {
		return 0, nil
	}
	if val == nil {
		str := fmt.Sprintf("meta key not found %s", key)
		return 0, txStoreError(ErrDatabase, str, nil)
	}

	return int32(byteOrder.Uint32(val)), nil
}

// putNumBlockTxRecords stores the no. of tx in the given block height
func putNumBlockTxRecords(tx walletdb.Tx, height int32, n uint32) error {
	bucket, err := tx.RootBucket().Bucket(metaBucketName).
		CreateBucketIfNotExists(uint32ToBytes(uint32(height)))
	if err != nil {
		return err
	}
	err = bucket.Put(numBlockTxRecordsName, uint32ToBytes(n))
	if err != nil {
		str := fmt.Sprintf("failed to store meta key '%s'", numBlockTxRecordsName)
		return txStoreError(ErrDatabase, str, err)
	}
	return nil
}

// fetchNumBlockTxRecords fetch the no. of tx in the given block height
func fetchNumBlockTxRecords(tx walletdb.Tx, height int32) (uint32, error) {
	bucket := tx.RootBucket().Bucket(metaBucketName).
		Bucket(uint32ToBytes(uint32(height)))

	if bucket == nil {
		return 0, nil
	}
	val := bucket.Get(numBlockTxRecordsName)
	// Return 0 if the metadata is uninitialized
	if val == nil {
		return 0, nil
	}
	if val == nil {
		str := fmt.Sprintf("meta key not found %s", numBlockTxRecordsName)
		return 0, txStoreError(ErrDatabase, str, nil)
	}

	return byteOrder.Uint32(val), nil
}

// fetchUnconfirmedTxRecord retrieves a unconfirmed tx record from
// the unconfirmed bucket based on the tx sha hash
func fetchUnconfirmedTxRecord(tx walletdb.Tx, hash *btcwire.ShaHash) (*txRecord,
	error) {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(txRecordsBucketName)

	key := shaHashToBytes(hash)
	val := bucket.Get(key)
	if val == nil {
		str := "txrecord not found"
		return nil, txStoreError(ErrTxRecordNotFound, str, nil)
	}

	return deserializeTxRecordRow(key, val)
}

// fetchAllUnconfirmedTxRecords retrieves all unconfirmed tx records from
// the unconfirmed bucket
func fetchAllUnconfirmedTxRecords(tx walletdb.Tx) ([]*txRecord, error) {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(txRecordsBucketName)

	n, err := fetchMeta(tx, numUnconfirmedRecordsName)
	if err != nil {
		return []*txRecord{}, err
	}
	records := make([]*txRecord, n)

	i := 0
	err = bucket.ForEach(func(k, v []byte) error {
		// Skip buckets.
		if v == nil {
			return nil
		}
		record, err := deserializeTxRecordRow(k, v)
		if err != nil {
			return err
		}
		records[i] = record
		i++
		return nil
	})
	if err != nil {
		return nil, maybeConvertDbError(err)
	}
	return records, nil
}

// putUnconfirmedTxRecord inserts an unconfirmed tx record to the unconfirmed
// bucket
func putUnconfirmedTxRecord(tx walletdb.Tx, t *txRecord) error {
	n, err := fetchMeta(tx, numUnconfirmedRecordsName)
	if err != nil {
		return err
	}
	n++
	if err := putMeta(tx, numUnconfirmedRecordsName, n); err != nil {
		str := fmt.Sprintf("failed to store meta key '%s'", numUnconfirmedRecordsName)
		return txStoreError(ErrDatabase, str, err)
	}
	return updateUnconfirmedTxRecord(tx, t)
}

// updateUnconfirmedTxRecord updates a unconfirmed tx record in the unconfirmed
// txrecords bucket
func updateUnconfirmedTxRecord(tx walletdb.Tx, t *txRecord) error {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(txRecordsBucketName)

	// Write the serialized txrecord keyed by the tx hash.
	serializedRow, err := serializeTxRecordRow(t)
	if err != nil {
		str := fmt.Sprintf("failed to serialize txrecord '%s'", t.tx.Sha())
		return txStoreError(ErrDatabase, str, err)
	}
	err = bucket.Put(shaHashToBytes(t.tx.Sha()), serializedRow)
	if err != nil {
		str := fmt.Sprintf("failed to store confirmed txrecord '%s'", t.tx.Sha())
		return txStoreError(ErrDatabase, str, err)
	}
	return nil
}

// deleteUnconfirmedTxRecord deletes an unconfirmed tx record from the
// unconfirmed bucket
func deleteUnconfirmedTxRecord(tx walletdb.Tx, hash *btcwire.ShaHash) error {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(txRecordsBucketName)
	if err := bucket.Delete(shaHashToBytes(hash)); err != nil {
		str := fmt.Sprintf("failed to delete tx record '%s'", hash)
		return txStoreError(ErrDatabase, str, err)
	}
	n, err := fetchMeta(tx, numUnconfirmedRecordsName)
	if err != nil {
		return err
	}
	n--
	if err := putMeta(tx, numUnconfirmedRecordsName, n); err != nil {
		return err
	}
	return nil
}

// deleteBlockOutPointSpender deletes a block output key from the
// spentblockoutpointidx bucket
func deleteBlockOutPointSpender(tx walletdb.Tx, op *btcwire.OutPoint,
	key *BlockOutputKey) error {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(spentBlockOutPointIdxBucketName)
	if err := bucket.Delete(blockOutputKeyToBytes(key)); err != nil {
		str := fmt.Sprintf("failed to delete output key spender '(%d, %d), %d'",
			key.BlockIndex, key.BlockHeight, key.OutputIndex)
		return txStoreError(ErrDatabase, str, err)
	}
	bucket = tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(spentBlockOutPointKeyIdxBucketName)
	if err := bucket.Delete(outPointToBytes(op)); err != nil {
		str := fmt.Sprintf("failed to delete outpoint spender '%s, %d'", op.Hash,
			op.Index)
		return txStoreError(ErrDatabase, str, err)
	}
	n, err := fetchMeta(tx, numConfirmedSpendsName)
	if err != nil {
		return err
	}
	n--
	if err := putMeta(tx, numConfirmedSpendsName, n); err != nil {
		return err
	}
	return nil
}

// setPrevOutPointSpender updates previous outpoints index in the
// prevoutpointidx bucket
func setPrevOutPointSpender(tx walletdb.Tx, op *btcwire.OutPoint,
	t *txRecord) error {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(prevOutPointIdxBucketName)

	if err := bucket.Put(outPointToBytes(op),
		shaHashToBytes(t.tx.Sha())); err != nil {
		str := fmt.Sprintf("failed to store previous outpoint '%s'", t.tx.Sha())
		return txStoreError(ErrDatabase, str, err)
	}
	return nil
}

// fetchPrevOutPointSpender retrieves a tx record from the prevoutpointidx
// bucket which spends the given outpoint
func fetchPrevOutPointSpender(tx walletdb.Tx, op *btcwire.OutPoint) (*txRecord,
	error) {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(prevOutPointIdxBucketName)

	key := outPointToBytes(op)
	val := bucket.Get(key)
	hash, err := btcwire.NewShaHash(val)
	if err != nil {
		return nil, err
	}
	return fetchTxRecord(tx, hash)
}

// deletePrevOutPointSpender deletes a outpoint from the prevoutpointidx bucket
func deletePrevOutPointSpender(tx walletdb.Tx, op *btcwire.OutPoint) error {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(prevOutPointIdxBucketName)
	if err := bucket.Delete(outPointToBytes(op)); err != nil {
		str := fmt.Sprintf("failed to prev outpoint spender '%s, %d'", op.Hash,
			op.Index)
		return txStoreError(ErrDatabase, str, err)
	}
	return nil
}

// fetchBlockOutPointSpender retrieves a tx record from the
// spentblockoutpointidx bucket which spends the given block output key
func fetchBlockOutPointSpender(tx walletdb.Tx, key *BlockOutputKey) (*txRecord,
	error) {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(spentBlockOutPointIdxBucketName)

	val := bucket.Get(blockOutputKeyToBytes(key))
	hash, err := btcwire.NewShaHash(val)
	if err != nil {
		return nil, err
	}
	return fetchUnconfirmedTxRecord(tx, hash)
}

// fetchSpentBlockOutPointKey retrieves a tx record from the
// spentblockoutpointkeyidx bucket which spends the given outpoint
func fetchSpentBlockOutPointKey(tx walletdb.Tx, op *btcwire.OutPoint) (
	*BlockOutputKey, error) {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(spentBlockOutPointKeyIdxBucketName)

	key := outPointToBytes(op)
	val := bucket.Get(key)
	return deserializeBlockOutputKeyRow(key, val)
}

// setUnconfirmedOutPointSpender updates the spent unconfirmed index in the
// spentunconfirmedidx bucket
func setUnconfirmedOutPointSpender(tx walletdb.Tx, op *btcwire.OutPoint,
	t *txRecord) error {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(spentUnconfirmedIdxBucketName)

	err := bucket.Put(outPointToBytes(op), shaHashToBytes(t.tx.Sha()))
	if err != nil {
		str := fmt.Sprintf("failed to store spent unconfirmed tx '%s'",
			t.tx.Sha())
		return txStoreError(ErrDatabase, str, err)
	}
	n, err := fetchMeta(tx, numUnconfirmedSpendsName)
	if err != nil {
		return err
	}
	n++
	if err := putMeta(tx, numUnconfirmedSpendsName, n); err != nil {
		return err
	}
	return nil
}

// fetchUnconfirmedSpends retrieves all the spent unconfirmed tx records from
// the spentunconfirmedidx bucket
func fetchUnconfirmedSpends(tx walletdb.Tx) ([]*txRecord, error) {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(spentUnconfirmedIdxBucketName)

	numUnconfirmedSpends, err := fetchMeta(tx, numUnconfirmedSpendsName)
	if err != nil {
		return []*txRecord{}, err
	}
	records := make([]*txRecord, numUnconfirmedSpends)

	i := 0
	err = bucket.ForEach(func(k, v []byte) error {
		// Skip buckets.
		if v == nil {
			return nil
		}

		hash, err := btcwire.NewShaHash(v)
		if err != nil {
			return err
		}
		record, err := fetchUnconfirmedTxRecord(tx, hash)
		if err != nil {
			return err
		}
		records[i] = record
		i++
		return nil
	})
	if err != nil {
		return nil, maybeConvertDbError(err)
	}
	return records, nil
}

// deleteUnconfirmedOutPointSpender deletes a outpoint from the
// spentunconfirmedidx bucket
func deleteUnconfirmedOutPointSpender(tx walletdb.Tx,
	op *btcwire.OutPoint) error {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(spentUnconfirmedIdxBucketName)
	if err := bucket.Delete(outPointToBytes(op)); err != nil {
		str := fmt.Sprintf("failed to delete unconfirmed outpoint '%s'", op)
		return txStoreError(ErrDatabase, str, err)
	}
	if err := putMeta(tx, numUnconfirmedSpendsName, -1); err != nil {
		return err
	}
	return nil
}

// fetchConfirmedSpends retrieves all the spent tx records from the
// spentblockoutpointidx bucket
func fetchConfirmedSpends(tx walletdb.Tx) ([]*txRecord, error) {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(spentBlockOutPointIdxBucketName)

	n, err := fetchMeta(tx, numConfirmedSpendsName)
	if err != nil {
		return nil, err
	}
	records := make([]*txRecord, n)

	i := 0
	err = bucket.ForEach(func(k, v []byte) error {
		// Skip buckets.
		if v == nil {
			return nil
		}

		hash, err := btcwire.NewShaHash(v)
		if err != nil {
			return err
		}
		record, err := fetchUnconfirmedTxRecord(tx, hash)
		if err != nil {
			return err
		}
		records[i] = record
		i++
		return nil
	})
	if err != nil {
		return nil, maybeConvertDbError(err)
	}
	return records, nil
}

// fetchAllSpentBlockOutPoints retrieves all the spent block outpoints from the
// spentblockoutpoint bucket
func fetchAllSpentBlockOutPoints(tx walletdb.Tx) ([]*BlockOutputKey, error) {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(spentBlockOutPointIdxBucketName)

	n, err := fetchMeta(tx, numConfirmedSpendsName)
	if err != nil {
		return nil, err
	}
	keys := make([]*BlockOutputKey, n)

	i := 0
	err = bucket.ForEach(func(k, v []byte) error {
		// Skip buckets.
		if v == nil {
			return nil
		}

		key, err := deserializeBlockOutputKeyRow(k, k)
		if err != nil {
			return err
		}
		keys[i] = key
		i++
		return nil
	})
	if err != nil {
		return nil, maybeConvertDbError(err)
	}
	return keys, nil
}

// setBlockOutPointSpender updates the spent block outpoint index in the
// spentblockoutpoint bucket
func setBlockOutPointSpender(tx walletdb.Tx, op *btcwire.OutPoint,
	key *BlockOutputKey, t *txRecord) error {
	bucket := tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(spentBlockOutPointIdxBucketName)
	err := bucket.Put(blockOutputKeyToBytes(key), shaHashToBytes(t.tx.Sha()))
	if err != nil {
		str := fmt.Sprintf("failed to store spent block outpoint index '%s'",
			t.tx.Sha())
		return txStoreError(ErrDatabase, str, err)
	}

	bucket = tx.RootBucket().Bucket(unconfirmedBucketName).
		Bucket(spentBlockOutPointKeyIdxBucketName)
	err = bucket.Put(outPointToBytes(op), blockOutputKeyToBytes(key))
	if err != nil {
		str := fmt.Sprintf("failed to store spent block outpoint key '%d'",
			key.BlockHeight)
		return txStoreError(ErrDatabase, str, err)
	}
	n, err := fetchMeta(tx, numConfirmedSpendsName)
	if err != nil {
		return err
	}
	n++
	if err := putMeta(tx, numConfirmedSpendsName, n); err != nil {
		return err
	}
	return nil
}

// updateBlockTxIdx updates the block tx index for the given block and tx
// record The indexes are used to lookup tx records belonging to a given block
func updateBlockTxIdx(tx walletdb.Tx, b *Block, t *btcutil.Tx) error {
	bucket, err := tx.RootBucket().Bucket(blockTxIdxBucketName).
		CreateBucketIfNotExists(uint32ToBytes(uint32(b.Height)))
	if err != nil {
		return err
	}

	if err = bucket.Put(shaHashToBytes(t.Sha()), []byte{0}); err != nil {
		str := fmt.Sprintf("failed to store block tx index key '%s'", t.Sha())
		return txStoreError(ErrDatabase, str, err)
	}

	bucket, err = tx.RootBucket().Bucket(blockTxKeyIdxBucketName).
		CreateBucketIfNotExists(uint32ToBytes(uint32(b.Height)))
	if err != nil {
		str := fmt.Sprintf("failed to create index bucket for block '%d'",
			b.Height)
		return txStoreError(ErrDatabase, str, err)
	}
	blockTxKey := new(BlockTxKey)
	blockTxKey.BlockHeight = b.Height
	blockTxKey.BlockIndex = t.Index()

	if err = bucket.Put(blockTxKeyToBytes(blockTxKey),
		shaHashToBytes(t.Sha())); err != nil {
		str := fmt.Sprintf("failed to store block index key '%s'", t.Sha())
		return txStoreError(ErrDatabase, str, err)
	}
	return nil
}

// deleteBlock deletes the block and it's associated indexes for the given
// block height
func deleteBlock(tx walletdb.Tx, height int32) error {
	bucket := tx.RootBucket().Bucket(blocksBucketName)
	key := uint32ToBytes(uint32(height))
	if err := bucket.Delete(key); err != nil {
		str := fmt.Sprintf("failed to delete block '%d'", height)
		return txStoreError(ErrDatabase, str, err)
	}
	bucket = tx.RootBucket().Bucket(blockTxIdxBucketName)
	if err := bucket.DeleteBucket(key); err != nil {
		str := fmt.Sprintf("failed to delete block tx index '%d'", height)
		return txStoreError(ErrDatabase, str, err)
	}
	bucket = tx.RootBucket().Bucket(blockTxKeyIdxBucketName)
	if err := bucket.DeleteBucket(key); err != nil {
		str := fmt.Sprintf("failed to delete block tx key index '%d'", height)
		return txStoreError(ErrDatabase, str, err)
	}
	// Update block metadata i.e. no. of tx in block and no. of blocks
	if err := putNumBlockTxRecords(tx, height, 0); err != nil {
		return err
	}
	n, err := fetchMeta(tx, numBlocksName)
	if err != nil {
		return err
	}
	n--
	if err := putMeta(tx, numBlocksName, n); err != nil {
		return err
	}
	return nil
}

// fetchTxHashFromBlockTxKey retrieves the tx hash from the block tx key index
// with the given block tx key. The tx record can be retrieved from the
// txrecords bucket using the tx hash
func fetchTxHashFromBlockTxKey(tx walletdb.Tx, key *BlockTxKey) (
	*btcwire.ShaHash, error) {
	bucket := tx.RootBucket().Bucket(blockTxKeyIdxBucketName).
		Bucket(uint32ToBytes(uint32(key.BlockHeight)))
	if bucket == nil {
		str := fmt.Sprintf("missing index for block tx key '%d, %d'",
			key.BlockHeight, key.BlockIndex)
		return nil, txStoreError(ErrTxHashNotFound, str, nil)
	}

	val := bucket.Get(blockTxKeyToBytes(key))
	if val == nil {
		str := fmt.Sprintf("missing tx hash for block tx key '%d, %d'",
			key.BlockHeight, key.BlockIndex)
		return nil, txStoreError(ErrTxHashNotFound, str, nil)
	}

	hash, err := btcwire.NewShaHash(val)
	if err != nil {
		return nil, err
	}
	return hash, nil
}

// fetchTxRecord retrieves a tx record from the txrecords bucket with the given
// hash
func fetchTxRecord(tx walletdb.Tx, hash *btcwire.ShaHash) (*txRecord, error) {
	bucket := tx.RootBucket().Bucket(txRecordsBucketName)

	key := shaHashToBytes(hash)
	val := bucket.Get(key)
	if val == nil {
		str := fmt.Sprintf("missing tx record for hash '%s'", hash.String())
		return nil, txStoreError(ErrTxRecordNotFound, str, nil)
	}

	return deserializeTxRecordRow(key, val)
}

// fetchBlockTxRecords retrieves all tx records from the txrecords bucket
// belonging to the block with the given height
func fetchBlockTxRecords(tx walletdb.Tx, height int32) ([]*txRecord, error) {
	bucket := tx.RootBucket().Bucket(txRecordsBucketName)

	blockBucket := tx.RootBucket().Bucket(blockTxIdxBucketName).
		Bucket(uint32ToBytes(uint32(height)))
	if blockBucket == nil {
		return nil, nil
	}

	n, err := fetchNumBlockTxRecords(tx, height)
	if err != nil {
		return nil, err
	}

	records := make([]*txRecord, n)

	i := 0
	err = blockBucket.ForEach(func(k, v []byte) error {
		serializedRow := bucket.Get(k)
		row, err := deserializeTxRecordRow(k, serializedRow)
		if err != nil {
			return err
		}
		records[i] = row
		i++
		return nil
	})
	if err != nil {
		return nil, maybeConvertDbError(err)
	}
	return records, nil
}

// putBlock inserts a block into the blocks bucket
func putBlock(tx walletdb.Tx, block *Block) error {
	n, err := fetchMeta(tx, numBlocksName)
	if err != nil {
		return err
	}
	n++
	if err := putMeta(tx, numBlocksName, n); err != nil {
		return err
	}
	return updateBlock(tx, block)
}

// updateBlock updates a block into the blocks bucket
func updateBlock(tx walletdb.Tx, block *Block) error {
	bucket := tx.RootBucket().Bucket(blocksBucketName)

	// Write the serialized block keyed by the block hash.
	serializedRow := serializeBlockRow(block)
	err := bucket.Put(uint32ToBytes(uint32(block.Height)), serializedRow)
	if err != nil {
		str := fmt.Sprintf("failed to store block '%s'", block.Hash)
		return txStoreError(ErrDatabase, str, err)
	}
	return nil
}

// fetchUnspentOutpoints returns all unspent outpoints from the unspent bucket
func fetchUnspentOutpoints(tx walletdb.Tx) (map[*btcwire.OutPoint]*BlockTxKey,
	error) {
	bucket := tx.RootBucket().Bucket(unspentBucketName)

	outpoints := make(map[*btcwire.OutPoint]*BlockTxKey)

	err := bucket.ForEach(func(k, v []byte) error {
		// Skip buckets.
		if v == nil {
			return nil
		}
		row, err := deserializeBlockTxKeyRow(k, v)
		if err != nil {
			return err
		}
		op, err := deserializeOutPoint(k)
		if err != nil {
			return err
		}
		outpoints[op] = row
		return nil
	})
	if err != nil {
		return nil, maybeConvertDbError(err)
	}
	return outpoints, nil
}

// fetchUnspent returns an unspent outpoint from the unspent bucket
func fetchUnspent(tx walletdb.Tx, op *btcwire.OutPoint) (*BlockTxKey, error) {
	bucket := tx.RootBucket().Bucket(unspentBucketName)

	key := outPointToBytes(op)
	val := bucket.Get(key)
	if val == nil {
		str := fmt.Sprintf("block tx key for outpoint '%s' not found", op)
		return nil, txStoreError(ErrBlockTxKeyNotFound, str, nil)
	}

	return deserializeBlockTxKeyRow(key, val)
}

// putUnspent inserts a given unspent outpoint into the unspent bucket
func putUnspent(tx walletdb.Tx, op *btcwire.OutPoint, k *BlockTxKey) error {
	bucket := tx.RootBucket().Bucket(unspentBucketName)

	// Write the serialized block tx key keyed by outpoint
	serializedRow := serializeBlockTxKeyRow(k)
	err := bucket.Put(outPointToBytes(op), serializedRow)
	if err != nil {
		str := fmt.Sprintf("failed to store unspent record '%s'", op.Hash)
		return txStoreError(ErrDatabase, str, err)
	}
	return nil
}

// deleteUnspent deletes a given unspent output from the unspent bucket
func deleteUnspent(tx walletdb.Tx, op *btcwire.OutPoint) error {
	bucket := tx.RootBucket().Bucket(unspentBucketName)

	err := bucket.Delete(outPointToBytes(op))
	if err != nil {
		str := fmt.Sprintf("failed to delete unspent key '%s'", op.Hash)
		return txStoreError(ErrDatabase, str, err)
	}
	return nil
}

// fetchBlockByHeight returns a block from the blocks bucket with the given
// height
func fetchBlockByHeight(tx walletdb.Tx, height int32) (*Block, error) {
	bucket := tx.RootBucket().Bucket(blocksBucketName)

	key := uint32ToBytes(uint32(height))
	val := bucket.Get(key)
	if val == nil {
		str := fmt.Sprintf("block '%d' not found", height)
		return nil, txStoreError(ErrBlockNotFound, str, nil)
	}

	return deserializeBlockRow(key, val)
}

// fetchAllBlocks returns all the blocks in the blocks bucket
func fetchAllBlocks(tx walletdb.Tx) ([]*Block, error) {
	bucket := tx.RootBucket().Bucket(blocksBucketName)

	n, err := fetchMeta(tx, numBlocksName)
	blocks := make([]*Block, n)

	i := 0
	err = bucket.ForEach(func(k, v []byte) error {
		// Skip buckets.
		if v == nil {
			return nil
		}
		block, err := deserializeBlockRow(k, v)
		if err != nil {
			return err
		}
		blocks[i] = block
		i++
		return nil
	})
	if err != nil {
		return nil, maybeConvertDbError(err)
	}
	return blocks, nil
}

// fetchBlocks returns blocks from the blocks bucket
// whose height is greater than or equal to the given height.
func fetchBlocks(tx walletdb.Tx, height int32) ([]*Block, error) {
	bucket := tx.RootBucket().Bucket(blocksBucketName)

	var blocks []*Block

	err := bucket.ForEach(func(k, v []byte) error {
		// Skip buckets.
		if v == nil {
			return nil
		}
		h := int32(byteOrder.Uint32(k))
		if h >= height {
			row, err := deserializeBlockRow(k, v)
			if err != nil {
				return err
			}
			blocks = append(blocks, row)
		}
		return nil
	})
	if err != nil {
		return blocks, maybeConvertDbError(err)
	}
	return blocks, nil
}

// deserializeTxRecordRow deserializes the passed serialized tx record
// information.
func deserializeTxRecordRow(k []byte, serializedTxRecord []byte) (*txRecord,
	error) {
	txRecord := new(txRecord)

	// Read transaction index (as a uint32).
	txIndex := int(byteOrder.Uint32(serializedTxRecord[0:4]))

	// Read MsgTx size (as a uint64).
	msgTxLen := int(byteOrder.Uint64(serializedTxRecord[4:12]))
	buf := bytes.NewBuffer(serializedTxRecord[12 : 12+msgTxLen])

	// Deserialize transaction.
	msgTx := new(btcwire.MsgTx)
	err := msgTx.Deserialize(buf)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}

	offset := msgTxLen + 12
	// Create and save the btcutil.Tx of the read MsgTx and set its index.
	tx := btcutil.NewTx((*btcwire.MsgTx)(msgTx))
	tx.SetIndex(txIndex)
	txRecord.tx = tx

	// Read identifier for existence of debits.
	hasDebits, err := byteMarksValidPointer(serializedTxRecord[offset])
	offset += 1
	if err != nil {
		return nil, err
	}

	// If debits have been set, read them.  Otherwise, set to nil.
	if hasDebits {
		// Read debited amount (int64).
		amount := btcutil.Amount(
			byteOrder.Uint64(serializedTxRecord[offset : offset+8]))
		offset += 8

		// Read number of written outputs (as a uint32) this record
		// debits from.
		spendsCount := byteOrder.Uint32(serializedTxRecord[offset : offset+4])
		offset += 4

		// For each expected output key, allocate and read the key,
		// appending the result to the spends slice.  This slice is
		// originally set empty (*not* preallocated to spendsCount
		// size) to prevent accidentally allocating so much memory that
		// the process dies.
		spends := make([]BlockOutputKey, spendsCount)
		for i := uint32(0); i < spendsCount; i++ {
			k, err := deserializeBlockOutputKeyRow(k,
				serializedTxRecord[offset:offset+12])
			if err != nil {
				return nil, err
			}
			offset += 12
			spends[i] = *k
		}

		txRecord.debits = &debits{amount, spends}
	} else {
		txRecord.debits = nil
	}

	// Read number of pointers (as a uint32) written to be read into the
	// credits slice (although some may be nil).
	creditsCount := byteOrder.Uint32(serializedTxRecord[offset : offset+4])
	offset += 4

	// For each expected credits slice element, check whether the credit
	// exists or the pointer is nil.  If nil, append nil to credits and
	// continue with the next.  If non-nil, allocated and read the full
	// credit structure.  This slice is originally set to nil (*not*
	// preallocated to creditsCount size) to prevent accidentally allocating
	// so much memory that the process dies.
	credits := make([]*credit, creditsCount)
	for i := uint32(0); i < creditsCount; i++ {
		// Read identifer for a valid pointer.
		validCredit, err := byteMarksValidPointer(serializedTxRecord[offset])
		offset += 1
		if err != nil {
			return nil, err
		}

		if !validCredit {
			credits[i] = nil
		} else {
			// Read single byte that specifies whether this credit
			// was added as change.
			change, err := byteAsBool(serializedTxRecord[offset])
			offset += 1
			if err != nil {
				return nil, err
			}

			// Read identifier for a valid pointer.
			validSpentBy, err := byteMarksValidPointer(
				serializedTxRecord[offset])
			offset += 1
			if err != nil {
				return nil, err
			}

			// If spentBy pointer is valid, allocate and read a
			// transaction lookup key.
			var spentBy *BlockTxKey
			if validSpentBy {
				spentBy, err = deserializeBlockTxKeyRow(k,
					serializedTxRecord[offset:offset+8])
				if err != nil {
					return nil, err
				}
				offset += 8
			}

			c := &credit{change, spentBy}
			credits[i] = c
		}

	}
	txRecord.credits = credits

	// Read received unix time (int64).
	received := int64(byteOrder.Uint64(serializedTxRecord[offset : offset+8]))
	offset += 8
	txRecord.received = time.Unix(received, 0)

	return txRecord, nil
}

// serializeTxRecordRow returns the serialization of the passed txrecord row.
func serializeTxRecordRow(row *txRecord) ([]byte, error) {
	msgTx := row.tx.MsgTx()
	n := int64(msgTx.SerializeSize())

	// fixed size
	size := 4 + 8
	// variable size
	size += int(n)
	if row.debits == nil {
		size += 1
	} else {
		size += 1 + 8 + 4 + 12*len(row.debits.spends)
	}
	size += 4
	for _, c := range row.credits {
		if c == nil {
			size += 1
		} else {
			size += 1 + 1
			if c.spentBy == nil {
				size += 1
			} else {
				size += 1 + 12
			}
		}
	}
	size += 8
	buf := make([]byte, size)

	// Write transaction index (as a uint32).
	byteOrder.PutUint32(buf[0:4], uint32(row.tx.Index()))

	// Write msgTx size (as a uint64).
	byteOrder.PutUint64(buf[4:12], uint64(n))

	// Serialize and write transaction.
	var b bytes.Buffer
	err := msgTx.Serialize(&b)
	if err != nil {
		return nil, err
	}
	copy(buf[12:12+n], b.Bytes())
	offset := n + 12

	// Write debit records, if any.  This begins with a single byte to
	// identify whether the record contains any debits or not.
	if row.debits == nil {
		// Write identifier for nil debits.
		buf[offset] = nilPointer
		offset += 1
	} else {
		// Write identifier for valid debits.
		buf[offset] = validPointer
		offset += 1

		// Write debited amount (int64).
		byteOrder.PutUint64(buf[offset:offset+8], uint64(row.debits.amount))
		offset += 8

		// Write number of outputs (as a uint32) this record debits
		// from.
		byteOrder.PutUint32(buf[offset:offset+4], uint32(len(row.debits.spends)))
		offset += 4

		// Write each lookup key for a spent transaction output.
		for _, k := range row.debits.spends {
			copy(buf[offset:offset+12], blockOutputKeyToBytes(&k))
			offset += 12
		}
	}

	// Write number of pointers (as a uint32) in the credits slice (although
	// some may be nil).  Then, for each element in the credits slice, write
	// an identifier whether the element is nil or valid, and if valid,
	// write the credit structure.
	byteOrder.PutUint32(buf[offset:offset+4], uint32(len(row.credits)))
	offset += 4
	for _, c := range row.credits {
		if c == nil {
			// Write identifier for nil credit.
			buf[offset] = nilPointer
			offset += 1
		} else {
			// Write identifier for valid credit.
			buf[offset] = validPointer
			offset += 1

			// Write a single byte to specify whether this credit
			// was added as change, plus an extra empty byte which
			// used to specify whether the credit was locked.  This
			// extra byte is currently unused and may be used for
			// other flags in the future.
			changeByte := falseByte
			if c.change {
				changeByte = trueByte
			}
			buf[offset] = changeByte
			offset += 1

			// If this credit is unspent, write an identifier for
			// an invalid pointer.  Otherwise, write the identifier
			// for a valid pointer and write the spending tx key.
			if c.spentBy == nil {
				// Write identifier for an unspent credit.
				buf[offset] = nilPointer
				offset += 1
			} else {
				// Write identifier for an unspent credit.
				buf[offset] = validPointer
				offset += 1

				// Write transaction lookup key.
				copy(buf[offset:offset+12], serializeBlockTxKeyRow(c.spentBy))
				offset += 12
			}
		}
	}

	// Write received unix time (int64).
	byteOrder.PutUint64(buf[offset:offset+8], uint64(row.received.Unix()))
	offset += 8
	return buf, nil
}

// serializeBlockTxKeyRow returns the serialization of the passed block tx key.
func serializeBlockTxKeyRow(row *BlockTxKey) []byte {
	buf := make([]byte, 8)

	// Write block index (as a uint32).
	byteOrder.PutUint32(buf[0:4], uint32(row.BlockIndex))
	// Write block height (int32).
	byteOrder.PutUint32(buf[4:8], uint32(row.BlockHeight))
	return buf
}

// deserializeBlockTxKeyRow deserializes the passed serialized block tx key
// information.
func deserializeBlockTxKeyRow(k []byte, serializedBlockTxKey []byte) (
	*BlockTxKey, error) {
	// The serialized block tx key format is:
	//   <blockindex><blockheight>
	//
	//   4 bytes block index + 4 bytes block height
	if len(serializedBlockTxKey) < 8 {
		str := fmt.Sprintf("malformed serialized block tx key for key %x",
			k)
		return nil, txStoreError(ErrDatabase, str, nil)
	}

	blockTxKey := new(BlockTxKey)

	// Read block index (as a uint32).
	blockTxKey.BlockIndex = int(byteOrder.Uint32(serializedBlockTxKey[0:4]))
	// Read block height (int32).
	blockTxKey.BlockHeight = int32(byteOrder.Uint32(serializedBlockTxKey[4:8]))
	return blockTxKey, nil
}

// deserializeBlockOutputKeyRow deserializes the passed serialized block output
// key information.
func deserializeBlockOutputKeyRow(k []byte, serializedBlockOutputKey []byte) (
	*BlockOutputKey, error) {
	// The serialized block output key format is:
	//   <blocktxkey><outputindex>
	//
	//   8 bytes block tx key + 4 bytes output index
	if len(serializedBlockOutputKey) < 12 {
		str := fmt.Sprintf("malformed serialized block output key for key %x",
			k)
		return nil, txStoreError(ErrDatabase, str, nil)
	}

	blockOutputKey := new(BlockOutputKey)
	// Read embedded BlockTxKey.
	blockTxKey, err := deserializeBlockTxKeyRow(k, serializedBlockOutputKey[0:8])
	if err != nil {
		return nil, err
	}
	blockOutputKey.BlockTxKey = *blockTxKey

	// Read output index (uint32).
	blockOutputKey.OutputIndex = byteOrder.Uint32(serializedBlockOutputKey[8:12])
	return blockOutputKey, nil
}

// upgradeManager opens the tx store using the specified namespace or creates
// and initializes it if it does not already exist.  It also provides
// facilities to upgrade the data in the namespace to newer versions.
func upgradeManager(namespace walletdb.Namespace) error {
	// Initialize the buckets and main db fields as needed.
	var version uint32
	var createDate uint64
	err := namespace.Update(func(tx walletdb.Tx) error {
		rootBucket := tx.RootBucket()
		mainBucket, err := rootBucket.CreateBucketIfNotExists(
			mainBucketName)
		if err != nil {
			str := "failed to create main bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		_, err = rootBucket.CreateBucketIfNotExists(blocksBucketName)
		if err != nil {
			str := "failed to create blocks bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		_, err = rootBucket.CreateBucketIfNotExists(txRecordsBucketName)
		if err != nil {
			str := "failed to create tx records bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		_, err = rootBucket.CreateBucketIfNotExists(blockTxIdxBucketName)
		if err != nil {
			str := "failed to create block tx index bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		_, err = rootBucket.CreateBucketIfNotExists(blockTxKeyIdxBucketName)
		if err != nil {
			str := "failed to create block tx key index bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		_, err = rootBucket.CreateBucketIfNotExists(unspentBucketName)
		if err != nil {
			str := "failed to create unspent bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		_, err = rootBucket.CreateBucketIfNotExists(metaBucketName)
		if err != nil {
			str := "failed to create meta bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		unconfirmedBucket, err := rootBucket.
			CreateBucketIfNotExists(unconfirmedBucketName)
		if err != nil {
			str := "failed to create unconfirmed store bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		_, err = unconfirmedBucket.CreateBucketIfNotExists(txRecordsBucketName)
		if err != nil {
			str := "failed to create unconfirmed tx records bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		_, err = unconfirmedBucket.
			CreateBucketIfNotExists(spentBlockOutPointIdxBucketName)
		if err != nil {
			str := "failed to create unconfirmed spent block outpoint index bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		_, err = unconfirmedBucket.
			CreateBucketIfNotExists(spentUnconfirmedIdxBucketName)
		if err != nil {
			str := "failed to create unconfirmed spent unconfirmed index bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		_, err = unconfirmedBucket.
			CreateBucketIfNotExists(prevOutPointIdxBucketName)
		if err != nil {
			str := "failed to create unconfirmed previous outpoint index bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		_, err = unconfirmedBucket.
			CreateBucketIfNotExists(spentBlockOutPointKeyIdxBucketName)
		if err != nil {
			str := "failed to create unconfirmed spent block outpoint key index bucket"
			return txStoreError(ErrDatabase, str, err)
		}

		// Save the most recent tx store version if it isn't already
		// there, otherwise keep track of it for potential upgrades.
		verBytes := mainBucket.Get(txstoreVersionName)
		if verBytes == nil {
			version = LatestTxStoreVersion

			var buf [4]byte
			byteOrder.PutUint32(buf[:], version)
			err := mainBucket.Put(txstoreVersionName, buf[:])
			if err != nil {
				str := "failed to store latest database version"
				return txStoreError(ErrDatabase, str, err)
			}
		} else {
			version = byteOrder.Uint32(verBytes)
		}

		createBytes := mainBucket.Get(txstoreCreateDateName)
		if createBytes == nil {
			createDate = uint64(time.Now().Unix())
			var buf [8]byte
			byteOrder.PutUint64(buf[:], createDate)
			err := mainBucket.Put(txstoreCreateDateName, buf[:])
			if err != nil {
				str := "failed to store database creation time"
				return txStoreError(ErrDatabase, str, err)
			}
		} else {
			createDate = byteOrder.Uint64(createBytes)
		}

		return nil
	})
	if err != nil {
		str := "failed to update database"
		return txStoreError(ErrDatabase, str, err)
	}

	// Upgrade the tx store as needed.
	if version < LatestTxStoreVersion {
		// No upgrades yet.
	}

	return nil
}
