package l2

import (
	"errors"
	"math/big"
	"sync"

	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/log"

	bridge_abi "scroll-tech/bridge/abi"
	"scroll-tech/bridge/sender"

	"scroll-tech/database/orm"
)

func (r *Layer2Relayer) commitInit() error {
	ids, err := r.db.GetBatchesByRollupStatus(orm.RollupCommitting, 10)
	if err != nil || len(ids) == 0 {
		return err
	}

	for _, id := range ids {
		txStr, err := r.db.GetCommitTxHash(id)
		if err != nil {
			log.Error("failed to get commit_tx_hash from block_batch", "err", err)
			continue
		}

		_, data, err := r.committedPack(id)
		if err != nil {
			log.Error("failed to load or send committed tx", "batch id", id, "err", err)
			continue
		}

		txID := id + "-commit"
		err = r.rollupSender.LoadOrSendTx(
			common.HexToHash(txStr.String),
			txID,
			&r.cfg.RollupContractAddress,
			big.NewInt(0),
			data,
		)
		if err != nil {
			log.Error("failed to load or send tx", "batch id", id, "err", err)
		} else {
			r.processingCommitment.Store(txID, id)
		}
	}
	return nil
}

func (r *Layer2Relayer) committedPack(id string) (*orm.BlockBatch, []byte, error) {
	batches, err := r.db.GetBlockBatches(map[string]interface{}{"id": id})
	if err != nil || len(batches) == 0 {
		log.Error("Failed to GetBlockBatches", "batch_id", id, "err", err)
		return nil, nil, err
	}
	batch := batches[0]

	traces, err := r.db.GetBlockTraces(map[string]interface{}{"batch_id": id}, "ORDER BY number ASC")
	if err != nil || len(traces) == 0 {
		log.Error("Failed to GetBlockTraces", "batch_id", id, "err", err)
		return nil, nil, err
	}

	layer2Batch := &bridge_abi.IZKRollupLayer2Batch{
		BatchIndex: batch.Index,
		ParentHash: common.HexToHash(batch.ParentHash),
		Blocks:     make([]bridge_abi.IZKRollupLayer2BlockHeader, len(traces)),
	}

	parentHash := common.HexToHash(batch.ParentHash)
	for i, trace := range traces {
		layer2Batch.Blocks[i] = bridge_abi.IZKRollupLayer2BlockHeader{
			BlockHash:   trace.Header.Hash(),
			ParentHash:  parentHash,
			BaseFee:     trace.Header.BaseFee,
			StateRoot:   trace.StorageTrace.RootAfter,
			BlockHeight: trace.Header.Number.Uint64(),
			GasUsed:     0,
			Timestamp:   trace.Header.Time,
			ExtraData:   make([]byte, 0),
			Txs:         make([]bridge_abi.IZKRollupLayer2Transaction, len(trace.Transactions)),
		}
		for j, tx := range trace.Transactions {
			layer2Batch.Blocks[i].Txs[j] = bridge_abi.IZKRollupLayer2Transaction{
				Caller:   tx.From,
				Nonce:    tx.Nonce,
				Gas:      tx.Gas,
				GasPrice: tx.GasPrice.ToInt(),
				Value:    tx.Value.ToInt(),
				Data:     common.Hex2Bytes(tx.Data),
				R:        tx.R.ToInt(),
				S:        tx.S.ToInt(),
				V:        tx.V.ToInt().Uint64(),
			}
			if tx.To != nil {
				layer2Batch.Blocks[i].Txs[j].Target = *tx.To
			}
			layer2Batch.Blocks[i].GasUsed += trace.ExecutionResults[j].Gas
		}

		// for next iteration
		parentHash = layer2Batch.Blocks[i].BlockHash
	}

	data, err := bridge_abi.RollupMetaABI.Pack("commitBatch", layer2Batch)
	if err != nil {
		log.Error("Failed to pack commitBatch", "id", id, "index", batch.Index, "err", err)
		return nil, nil, err
	}
	return batch, data, nil
}

// ProcessPendingBatches submit batch data to layer 1 rollup contract
func (r *Layer2Relayer) ProcessPendingBatches(wg *sync.WaitGroup) {
	defer wg.Done()
	// batches are sorted by batch index in increasing order
	batchesInDB, err := r.db.GetPendingBatches(1)
	if err != nil {
		log.Error("Failed to fetch pending L2 batches", "err", err)
		return
	}
	if len(batchesInDB) == 0 {
		return
	}
	id := batchesInDB[0]
	// @todo add support to relay multiple batches

	batch, data, err := r.committedPack(id)
	if err != nil {
		return
	}

	txID := id + "-commit"
	// add suffix `-commit` to avoid duplication with finalize tx in unit tests
	hash, err := r.rollupSender.SendTransaction(txID, &r.cfg.RollupContractAddress, big.NewInt(0), data)
	if err != nil {
		if !errors.Is(err, sender.ErrNoAvailableAccount) {
			log.Error("Failed to send commitBatch tx to layer1 ", "id", id, "index", batch.Index, "err", err)
		}
		return
	}
	log.Info("commitBatch in layer1", "batch_id", id, "index", batch.Index, "hash", hash)

	// record and sync with db, @todo handle db error
	err = r.db.UpdateCommitTxHashAndRollupStatus(r.ctx, id, hash.String(), orm.RollupCommitting)
	if err != nil {
		log.Error("UpdateCommitTxHashAndRollupStatus failed", "id", id, "index", batch.Index, "err", err)
	}
	r.processingCommitment.Store(txID, id)
}