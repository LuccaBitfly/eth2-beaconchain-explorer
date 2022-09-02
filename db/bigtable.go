package db

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"eth2-exporter/erc1155"
	"eth2-exporter/erc20"
	"eth2-exporter/erc721"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"strconv"

	gcp_bigtable "cloud.google.com/go/bigtable"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ethereum/go-ethereum/common"
	eth_types "github.com/ethereum/go-ethereum/core/types"
	"github.com/golang/protobuf/proto"
	"github.com/karlseguin/ccache/v2"
	"github.com/sirupsen/logrus"
)

var ErrBlockNotFound = errors.New("block not found")
var BigtableClient *Bigtable

type IndexFilter string

const (
	FILTER_TIME           IndexFilter = "TIME"
	FILTER_TO             IndexFilter = "TO"
	FILTER_FROM           IndexFilter = "FROM"
	FILTER_TOKEN_RECEIVED IndexFilter = "TOKEN_RECEIVED"
	FILTER_TOKEN_SENT     IndexFilter = "TOKEN_SENT"
	FILTER_METHOD         IndexFilter = "METHOD"
	FILTER_CONTRACT       IndexFilter = "CONTRACT"
	FILTER_ERROR          IndexFilter = "ERROR"
)

const max_block_number = 1000000000
const (
	DATA_COLUMN             = "d"
	INDEX_COLUMN            = "i"
	DEFAULT_FAMILY          = "f"
	DEFAULT_FAMILY_BLOCKS   = "default"
	ACCOUNT_METADATA_FAMILY = "a"
	ERC20_METADATA_FAMILY   = "erc20"
	ERC721_METADATA_FAMILY  = "erc721"
	ERC1155_METADATA_FAMILY = "erc1155"
	writeRowLimit           = 10000
	MAX_INT                 = 9223372036854775807
	MIN_INT                 = -9223372036854775808
)

var ZERO_ADDRESS []byte = []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

var (
	ERC20TOPIC   []byte
	ERC721TOPIC  []byte
	ERC1155Topic []byte
)

type Bigtable struct {
	client               *gcp_bigtable.Client
	tableData            *gcp_bigtable.Table
	tableBlocks          *gcp_bigtable.Table
	tableMetadataUpdates *gcp_bigtable.Table
	tableMetadata        *gcp_bigtable.Table
	chainId              string
}

func NewBigtable(project, instance, chainId string) (*Bigtable, error) {
	poolSize := 50
	btClient, err := gcp_bigtable.NewClient(context.Background(), project, instance, option.WithGRPCConnectionPool(poolSize))
	// btClient, err := gcp_bigtable.NewClient(context.Background(), project, instance)

	if err != nil {
		return nil, err
	}

	bt := &Bigtable{
		client:               btClient,
		tableData:            btClient.Open("data"),
		tableBlocks:          btClient.Open("blocks"),
		tableMetadataUpdates: btClient.Open("metadata_updates"),
		tableMetadata:        btClient.Open("metadata"),
		chainId:              chainId,
	}
	return bt, nil
}

func (bigtable *Bigtable) Close() {
	bigtable.client.Close()
}

func (bigtable *Bigtable) GetDataTable() *gcp_bigtable.Table {
	return bigtable.tableData
}

func (bigtable *Bigtable) GetMetadataUpdatesTable() *gcp_bigtable.Table {
	return bigtable.tableMetadataUpdates
}

func (bigtable *Bigtable) GetMetadatTable() *gcp_bigtable.Table {
	return bigtable.tableMetadata
}

func (bigtable *Bigtable) SaveBlock(block *types.Eth1Block) error {

	encodedBc, err := proto.Marshal(block)

	if err != nil {
		return err
	}
	ts := gcp_bigtable.Timestamp(0)

	mut := gcp_bigtable.NewMutation()
	mut.Set(DEFAULT_FAMILY_BLOCKS, "data", ts, encodedBc)

	err = bigtable.tableBlocks.Apply(context.Background(), fmt.Sprintf("%s:%s", bigtable.chainId, reversedPaddedBlockNumber(block.Number)), mut)

	if err != nil {
		return err
	}
	return nil
}

func (bigtable *Bigtable) SaveBlocks(block *types.Eth1Block) error {

	encodedBc, err := proto.Marshal(block)

	if err != nil {
		return err
	}
	ts := gcp_bigtable.Timestamp(0)

	mut := gcp_bigtable.NewMutation()
	mut.Set(DEFAULT_FAMILY, "data", ts, encodedBc)

	err = bigtable.tableBlocks.Apply(context.Background(), fmt.Sprintf("%s:%s", bigtable.chainId, reversedPaddedBlockNumber(block.Number)), mut)

	if err != nil {
		return err
	}
	return nil
}

func (bigtable *Bigtable) GetBlockFromBlocksTable(number uint64) (*types.Eth1Block, error) {

	paddedNumber := reversedPaddedBlockNumber(number)

	row, err := bigtable.tableBlocks.ReadRow(context.Background(), fmt.Sprintf("1:%s", paddedNumber))

	if err != nil {
		return nil, err
	}

	if len(row[DEFAULT_FAMILY_BLOCKS]) == 0 { // block not found
		logger.Errorf("block %v not found in block table", number)
		return nil, ErrBlockNotFound
	}

	bc := &types.Eth1Block{}
	err = proto.Unmarshal(row[DEFAULT_FAMILY_BLOCKS][0].Value, bc)

	if err != nil {
		return nil, err
	}

	return bc, nil
}

func (bigtable *Bigtable) CheckForGapsInBlocksTable(lookback int) (gapFound bool, start int, end int, err error) {

	prefix := bigtable.chainId + ":"
	previous := 0
	i := 0
	err = bigtable.tableBlocks.ReadRows(context.Background(), gcp_bigtable.PrefixRange(prefix), func(r gcp_bigtable.Row) bool {
		c, err := strconv.Atoi(strings.Replace(r.Key(), prefix, "", 1))

		if err != nil {
			logger.Errorf("error parsing block number from key %v: %v", r.Key(), err)
			return false
		}
		c = max_block_number - c

		if c%10000 == 0 {
			logger.Infof("scanning, currently at block %v", c)
		}

		if previous != 0 && previous != c+1 {
			gapFound = true
			start = c
			end = previous
			logger.Fatalf("found gap between block %v and block %v in blocks table", previous, c)
			return false
		}
		previous = c

		i++

		return i < lookback
	}, gcp_bigtable.RowFilter(gcp_bigtable.StripValueFilter()))

	return gapFound, start, end, err
}

func (bigtable *Bigtable) GetLastBlockInBlocksTable() (int, error) {
	prefix := bigtable.chainId + ":"
	lastBlock := 0
	err := bigtable.tableBlocks.ReadRows(context.Background(), gcp_bigtable.PrefixRange(prefix), func(r gcp_bigtable.Row) bool {
		c, err := strconv.Atoi(strings.Replace(r.Key(), prefix, "", 1))

		if err != nil {
			logger.Errorf("error parsing block number from key %v: %v", r.Key(), err)
			return false
		}
		c = max_block_number - c

		if c%10000 == 0 {
			logger.Infof("scanning, currently at block %v", c)
		}

		lastBlock = c
		return false
	}, gcp_bigtable.RowFilter(gcp_bigtable.StripValueFilter()))

	if err != nil {
		return 0, err
	}

	return lastBlock, nil
}

func (bigtable *Bigtable) CheckForGapsInDataTable(lookback int) error {

	prefix := bigtable.chainId + ":B:"
	previous := 0
	i := 0
	err := bigtable.tableData.ReadRows(context.Background(), gcp_bigtable.PrefixRange(prefix), func(r gcp_bigtable.Row) bool {
		c, err := strconv.Atoi(strings.Replace(r.Key(), prefix, "", 1))

		if err != nil {
			logger.Errorf("error parsing block number from key %v: %v", r.Key(), err)
			return false
		}
		c = max_block_number - c

		if c%10000 == 0 {
			logger.Infof("scanning, currently at block %v", c)
		}

		if previous != 0 && previous != c+1 {
			logger.Fatalf("found gap between block %v and block %v in data table", previous, c)
		}
		previous = c

		i++

		return i < lookback
	}, gcp_bigtable.RowFilter(gcp_bigtable.StripValueFilter()))

	if err != nil {
		return err
	}

	return nil
}

func (bigtable *Bigtable) GetLastBlockInDataTable() (int, error) {
	prefix := bigtable.chainId + ":B:"
	lastBlock := 0
	err := bigtable.tableData.ReadRows(context.Background(), gcp_bigtable.PrefixRange(prefix), func(r gcp_bigtable.Row) bool {
		c, err := strconv.Atoi(strings.Replace(r.Key(), prefix, "", 1))

		if err != nil {
			logger.Errorf("error parsing block number from key %v: %v", r.Key(), err)
			return false
		}
		c = max_block_number - c

		if c%10000 == 0 {
			logger.Infof("scanning, currently at block %v", c)
		}

		lastBlock = c
		return false
	}, gcp_bigtable.RowFilter(gcp_bigtable.StripValueFilter()))

	if err != nil {
		return 0, err
	}

	return lastBlock, nil
}

func (bigtable *Bigtable) GetFullBlockFromDataTable(number uint64) (*types.Eth1Block, error) {

	paddedNumber := reversedPaddedBlockNumber(number)

	row, err := bigtable.tableData.ReadRow(context.Background(), fmt.Sprintf("1:%s", paddedNumber))

	if err != nil {
		return nil, err
	}

	if len(row[DEFAULT_FAMILY]) == 0 { // block not found
		logger.Errorf("block %v not found in data table", number)
		return nil, ErrBlockNotFound
	}
	blocks := make([]*types.Eth1Block, 0, 1)
	rowHandler := getFullBlockHandler(&blocks)

	rowHandler(row)

	if err != nil {
		return nil, err
	}

	return blocks[0], nil
}

func (bigtable *Bigtable) GetMostRecentBlockFromDataTable() (*types.Eth1BlockIndexed, error) {
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	prefix := fmt.Sprintf("%s:B:", bigtable.chainId)

	rowRange := gcp_bigtable.PrefixRange(prefix)
	rowFilter := gcp_bigtable.RowFilter(gcp_bigtable.ColumnFilter("d"))
	limit := gcp_bigtable.LimitRows(1)

	block := types.Eth1BlockIndexed{}
	rowHandler := func(row gcp_bigtable.Row) bool {
		err := proto.Unmarshal(row[DEFAULT_FAMILY][0].Value, &block)
		if err != nil {
			logger.Errorf("error could not unmarschal proto object, err: %v", err)
		}

		return true
	}

	err := bigtable.tableData.ReadRows(ctx, rowRange, rowHandler, rowFilter, limit)
	if err != nil {
		return nil, err
	}

	return &block, nil
}

func getBlockHandler(blocks *[]*types.Eth1BlockIndexed) func(gcp_bigtable.Row) bool {
	return func(row gcp_bigtable.Row) bool {
		// startTime := time.Now()
		block := types.Eth1BlockIndexed{}
		err := proto.Unmarshal(row[DEFAULT_FAMILY][0].Value, &block)
		if err != nil {
			logger.Errorf("error could not unmarschal proto object, err: %v", err)
		}
		*blocks = append(*blocks, &block)
		// logger.Infof("finished processing row from table blocks: %v", time.Since(startTime))
		return true
	}
}

func getFullBlockHandler(blocks *[]*types.Eth1Block) func(gcp_bigtable.Row) bool {
	return func(row gcp_bigtable.Row) bool {
		// startTime := time.Now()
		block := types.Eth1Block{}
		err := proto.Unmarshal(row[DEFAULT_FAMILY][0].Value, &block)
		if err != nil {
			logger.Errorf("error could not unmarschal proto object, err: %v", err)
		}
		if len(row[DEFAULT_FAMILY]) > 1 {
			logs := make(map[string][]*types.Eth1Log, 100)
			itxs := make(map[string][]*types.Eth1InternalTransaction, 100)
			for _, item := range row[DEFAULT_FAMILY][1:] {
				if strings.HasPrefix(item.Column, "d:itx") {
					hash := strings.Split(item.Column, ":")[2]
					itx := types.Eth1InternalTransaction{}
					err := proto.Unmarshal(item.Value, &itx)
					if err != nil {
						logger.Errorf("error could not unmarschal proto object, err: %v", err)
					}
					itxs[hash] = append(itxs[hash], &itx)
				}
				if strings.HasPrefix(item.Column, "d:log") {
					hash := strings.Split(item.Column, ":")[2]
					log := types.Eth1Log{}
					err := proto.Unmarshal(item.Value, &log)
					if err != nil {
						logger.Errorf("error could not unmarschal proto object, err: %v", err)
					}
					logs[hash] = append(logs[hash], &log)
				}
				if strings.HasPrefix(item.Column, "d:tx") {
					hash := strings.Split(item.Column, ":")[3]
					tx := types.Eth1Transaction{}
					err := proto.Unmarshal(item.Value, &tx)
					if err != nil {
						logger.Errorf("error could not unmarschal proto object, err: %v", err)
					}
					tx.Logs = logs[hash]
					tx.Itx = itxs[hash]
					block.Transactions = append(block.Transactions, &tx)
				}
			}

		}

		*blocks = append(*blocks, &block)
		// logger.Infof("finished processing row from table stefan: %v", time.Since(startTime))

		return true
	}
}

// GetFullBlockDescending gets blocks starting at block start
func (bigtable *Bigtable) GetFullBlockDescending(start, limit uint64) ([]*types.Eth1Block, error) {
	startPadded := reversedPaddedBlockNumber(start)
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	prefix := fmt.Sprintf("%s:%s", bigtable.chainId, startPadded)

	rowRange := gcp_bigtable.InfiniteRange(prefix) //gcp_bigtable.PrefixRange("1:1000000000")

	blocks := make([]*types.Eth1Block, 0, 100)

	rowHandler := getFullBlockHandler(&blocks)

	startTime := time.Now()
	err := bigtable.tableData.ReadRows(ctx, rowRange, rowHandler, gcp_bigtable.LimitRows(int64(limit)))
	if err != nil {
		return nil, err
	}

	logger.Infof("finished getting blocks from table stefan: %v", time.Since(startTime))
	return blocks, nil
}

// GetBlocksDescending gets blocks starting at block start
func (bigtable *Bigtable) GetBlocksDescending(start, limit uint64) ([]*types.Eth1BlockIndexed, error) {
	startPadded := reversedPaddedBlockNumber(start)
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	prefix := fmt.Sprintf("%s:B:%s", bigtable.chainId, startPadded)

	rowRange := gcp_bigtable.InfiniteRange(prefix) //gcp_bigtable.PrefixRange("1:1000000000")
	rowFilter := gcp_bigtable.RowFilter(gcp_bigtable.ColumnFilter("d"))

	blocks := make([]*types.Eth1BlockIndexed, 0, 100)

	rowHandler := getBlockHandler(&blocks)

	startTime := time.Now()
	err := bigtable.tableData.ReadRows(ctx, rowRange, rowHandler, rowFilter, gcp_bigtable.LimitRows(int64(limit)))
	if err != nil {
		return nil, err
	}

	logger.Infof("finished getting blocks from table stefan: %v", time.Since(startTime))
	return blocks, nil
}

func reversedPaddedBlockNumber(blockNumber uint64) string {
	return fmt.Sprintf("%09d", max_block_number-blockNumber)
}

func reversePaddedBigtableTimestamp(timestamp *timestamppb.Timestamp) string {
	if timestamp == nil {
		log.Fatalf("unknown timestap: %v", timestamp)
	}
	return fmt.Sprintf("%019d", MAX_INT-timestamp.Seconds)
}

func reversePaddedIndex(i int, maxValue int) string {
	if i > maxValue {
		logrus.Fatal("padded index %v is greater than the max index of %v", maxValue)
	}
	length := fmt.Sprintf("%d", len(fmt.Sprintf("%d", maxValue))-1)
	fmtStr := "%0" + length + "d"
	return fmt.Sprintf(fmtStr, maxValue-i)
}

func blockFromPaddedBlockNumber(paddedBlockNumber string) uint64 {
	num := strings.Split(paddedBlockNumber, ":")
	paddedNumber, err := strconv.ParseUint(num[1], 10, 64)
	if err != nil {
		logger.WithError(err).Error("error parsing padded block")
		return 0
	}

	return uint64(max_block_number) - paddedNumber
}

func TimestampToBigtableTimeDesc(ts time.Time) string {
	return fmt.Sprintf("%04d%02d%02d%02d%02d%02d", 9999-ts.Year(), 12-ts.Month(), 31-ts.Day(), 23-ts.Hour(), 59-ts.Minute(), 59-ts.Second())
}

func (bigtable *Bigtable) WriteBulk(mutations *types.BulkMutations, table *gcp_bigtable.Table) error {
	length := 10000
	numMutations := len(mutations.Muts)
	numKeys := len(mutations.Keys)
	iterations := numKeys / length

	if numKeys != numMutations {
		return fmt.Errorf("error expected same number of keys as mutations keys: %v mutations: %v", numKeys, numMutations)
	}

	for offset := 0; offset < iterations; offset++ {
		start := offset * length
		end := offset*length + length
		// logger.Infof("writing from: %v to %v arr len:  %v", start, end, len(mutations.Keys))
		ctx, done := context.WithTimeout(context.Background(), time.Second*30)
		defer done()
		// startTime := time.Now()
		errs, err := table.ApplyBulk(ctx, mutations.Keys[start:end], mutations.Muts[start:end])
		for _, e := range errs {
			if e != nil {
				return err
			}
		}
		// logrus.Infof("wrote from %v to %v rows to bigtable in %.1f s", start, end, time.Since(startTime).Seconds())
		if err != nil {
			return err
		}
	}

	if (iterations * length) < numKeys {
		start := iterations * length

		ctx, done := context.WithTimeout(context.Background(), time.Second*30)
		defer done()
		// startTime := time.Now()
		errs, err := table.ApplyBulk(ctx, mutations.Keys[start:], mutations.Muts[start:])
		if err != nil {
			return err
		}
		for _, e := range errs {
			if e != nil {
				return e
			}
		}
		// logrus.Infof("wrote from %v to %v rows to bigtable in %.1fs", start, numKeys, time.Since(startTime).Seconds())
		if err != nil {
			return err
		}
		return nil
	}

	return nil

	// if err := g.Wait(); err == nil {
	// 	// logrus.Info("Successfully wrote all mutations")
	// 	return nil
	// } else {
	// 	return err
	// }
}

func (bigtable *Bigtable) DeleteRowsWithPrefix(prefix string) {

	for {
		ctx, done := context.WithTimeout(context.Background(), time.Second*30)
		defer done()

		rr := gcp_bigtable.InfiniteRange(prefix)

		rowsToDelete := make([]string, 0, 10000)
		bigtable.tableData.ReadRows(ctx, rr, func(r gcp_bigtable.Row) bool {
			rowsToDelete = append(rowsToDelete, r.Key())
			return true
		})
		mut := gcp_bigtable.NewMutation()
		mut.DeleteRow()

		muts := make([]*gcp_bigtable.Mutation, 0)
		for j := 0; j < 10000; j++ {
			muts = append(muts, mut)
		}

		l := len(rowsToDelete)
		if l == 0 {
			logger.Infof("all done")
			break
		}
		logger.Infof("deleting %v rows", l)

		for i := 0; i < l; i++ {
			if !strings.HasPrefix(rowsToDelete[i], "1:t:") {
				logger.Infof("wrong prefix: %v", rowsToDelete[i])
			}
			ctx, done := context.WithTimeout(context.Background(), time.Second*30)
			defer done()
			if i%10000 == 0 && i != 0 {
				logger.Infof("deleting rows: %v to %v", i-10000, i)
				errs, err := bigtable.tableData.ApplyBulk(ctx, rowsToDelete[i-10000:i], muts)
				if err != nil {
					logger.WithError(err).Errorf("error deleting row: %v", rowsToDelete[i])
				}
				for _, err := range errs {
					logger.Error(err)
				}
			}
			if l < 10000 && l > 0 {
				logger.Infof("deleting remainder")
				errs, err := bigtable.tableData.ApplyBulk(ctx, rowsToDelete, muts[:len(rowsToDelete)])
				if err != nil {
					logger.WithError(err).Errorf("error deleting row: %v", rowsToDelete[i])
				}
				for _, err := range errs {
					logger.Error(err)
				}
				break
			}
		}
	}

}

// TransformBlock extracts blocks from bigtable more specifically from the table blocks.
// It transforms the block and strips any information that is not necessary for a blocks view
// It writes blocks to table data:
// Row:    <chainID>:B:<reversePaddedBlockNumber>
// Family: f
// Column: data
// Cell:   Proto<Eth1BlockIndexed>
//
// It indexes blocks by:
// Row:    <chainID>:I:B:<Miner>:<reversePaddedBlockNumber>
// Family: f
// Column: <chainID>:B:<reversePaddedBlockNumber>
// Cell:   nil
func (bigtable *Bigtable) TransformBlock(block *types.Eth1Block, cache *ccache.Cache) (bulkData *types.BulkMutations, bulkMetadataUpdates *types.BulkMutations, err error) {

	bulkData = &types.BulkMutations{}
	bulkMetadataUpdates = &types.BulkMutations{}

	idx := types.Eth1BlockIndexed{
		Hash:       block.GetHash(),
		ParentHash: block.GetParentHash(),
		UncleHash:  block.GetUncleHash(),
		Coinbase:   block.GetCoinbase(),
		Difficulty: block.GetDifficulty(),
		Number:     block.GetNumber(),
		GasLimit:   block.GetGasLimit(),
		GasUsed:    block.GetGasUsed(),
		Time:       block.GetTime(),
		BaseFee:    block.GetBaseFee(),
		// Duration:               uint64(block.GetTime().AsTime().Unix() - previous.GetTime().AsTime().Unix()),
		UncleCount:       uint64(len(block.GetUncles())),
		TransactionCount: uint64(len(block.GetTransactions())),
		// BaseFeeChange:          new(big.Int).Sub(new(big.Int).SetBytes(block.GetBaseFee()), new(big.Int).SetBytes(previous.GetBaseFee())).Bytes(),
		// BlockUtilizationChange: new(big.Int).Sub(new(big.Int).Div(big.NewInt(int64(block.GetGasUsed())), big.NewInt(int64(block.GetGasLimit()))), new(big.Int).Div(big.NewInt(int64(previous.GetGasUsed())), big.NewInt(int64(previous.GetGasLimit())))).Bytes(),
	}

	uncleReward := big.NewInt(0)
	r := new(big.Int)

	for _, uncle := range block.Uncles {
		r.Add(big.NewInt(int64(uncle.GetNumber())), big.NewInt(8))
		r.Sub(r, big.NewInt(int64(block.GetNumber())))
		r.Mul(r, utils.BlockReward(block.GetNumber()))
		r.Div(r, big.NewInt(8))

		r.Div(utils.BlockReward(block.GetNumber()), big.NewInt(32))
		uncleReward.Add(uncleReward, r)
	}

	idx.UncleReward = uncleReward.Bytes()

	var maxGasPrice *big.Int
	var minGasPrice *big.Int
	txReward := big.NewInt(0)

	for _, t := range block.GetTransactions() {
		price := new(big.Int).SetBytes(t.GasPrice)

		if minGasPrice == nil {
			minGasPrice = price
		}
		if maxGasPrice == nil {
			maxGasPrice = price
		}

		if price.Cmp(maxGasPrice) > 0 {
			maxGasPrice = price
		}

		if price.Cmp(minGasPrice) < 0 {
			minGasPrice = price
		}

		txReward.Add(new(big.Int).Mul(big.NewInt(int64(t.GasUsed)), new(big.Int).SetBytes(t.GasPrice)), txReward)
	}

	idx.TxReward = txReward.Bytes()

	if maxGasPrice != nil {
		idx.LowestGasPrice = minGasPrice.Bytes()

	}
	if minGasPrice != nil {
		idx.HighestGasPrice = maxGasPrice.Bytes()
	}

	idx.Mev = CalculateMevFromBlock(block).Bytes()

	// Mark Coinbase for balance update
	markBalanceUpdate(idx.Coinbase, []byte{0x0}, bulkMetadataUpdates, cache)

	// <chainID>:b:<reverse number>
	key := fmt.Sprintf("%s:B:%s", bigtable.chainId, reversedPaddedBlockNumber(block.GetNumber()))
	mut := gcp_bigtable.NewMutation()

	b, err := proto.Marshal(&idx)
	if err != nil {
		return nil, nil, fmt.Errorf("error marshalling proto object err: %w", err)
	}

	mut.Set(DEFAULT_FAMILY, DATA_COLUMN, gcp_bigtable.Timestamp(0), b)

	bulkData.Keys = append(bulkData.Keys, key)
	bulkData.Muts = append(bulkData.Muts, mut)

	indexes := []string{
		// Index blocks by the miners address
		fmt.Sprintf("%s:I:B:%x:TIME:%s", bigtable.chainId, block.GetCoinbase(), reversePaddedBigtableTimestamp(block.Time)),
	}

	for _, idx := range indexes {
		mut := gcp_bigtable.NewMutation()
		mut.Set(DEFAULT_FAMILY, key, gcp_bigtable.Timestamp(0), nil)

		bulkData.Keys = append(bulkData.Keys, idx)
		bulkData.Muts = append(bulkData.Muts, mut)
	}

	return bulkData, bulkMetadataUpdates, nil
}

func CalculateMevFromBlock(block *types.Eth1Block) *big.Int {
	mevReward := big.NewInt(0)

	for _, tx := range block.GetTransactions() {
		for _, itx := range tx.GetItx() {
			//log.Printf("%v - %v", common.HexToAddress(itx.To), common.HexToAddress(block.Miner))
			if common.BytesToAddress(itx.To) == common.BytesToAddress(block.GetCoinbase()) {
				mevReward = new(big.Int).Add(mevReward, new(big.Int).SetBytes(itx.GetValue()))
			}
		}

	}
	return mevReward
}

// TransformTx extracts transactions from bigtable more specifically from the table blocks.
func (bigtable *Bigtable) TransformTx(blk *types.Eth1Block, cache *ccache.Cache) (bulkData *types.BulkMutations, bulkMetadataUpdates *types.BulkMutations, err error) {
	bulkData = &types.BulkMutations{}
	bulkMetadataUpdates = &types.BulkMutations{}

	for i, tx := range blk.Transactions {
		if i > 9999 {
			return nil, nil, fmt.Errorf("unexpected number of transactions in block expected at most 9999 but got: %v, tx: %x", i, tx.GetHash())
		}
		iReverse := reversePaddedIndex(i, 10000)
		// logger.Infof("address to: %x address: contract: %x, len(to): %v, len(contract): %v, contranct zero: %v", tx.GetTo(), tx.GetContractAddress(), len(tx.GetTo()), len(tx.GetContractAddress()), bytes.Equal(tx.GetContractAddress(), ZERO_ADDRESS))
		to := tx.GetTo()
		isContract := false
		if !bytes.Equal(tx.GetContractAddress(), ZERO_ADDRESS) {
			to = tx.GetContractAddress()
			isContract = true
		}
		// logger.Infof("sending to: %x", to)
		invokesContract := false
		if len(tx.GetItx()) > 0 || tx.GetGasUsed() > 21000 || tx.GetErrorMsg() != "" {
			invokesContract = true
		}
		method := make([]byte, 0)
		if len(tx.GetData()) > 3 {
			method = tx.GetData()[:4]
		}

		key := fmt.Sprintf("%s:TX:%x", bigtable.chainId, tx.GetHash())
		fee := new(big.Int).Mul(new(big.Int).SetBytes(tx.GetGasPrice()), big.NewInt(int64(tx.GetGasUsed()))).Bytes()
		indexedTx := &types.Eth1TransactionIndexed{
			Hash:               tx.GetHash(),
			BlockNumber:        blk.GetNumber(),
			Time:               blk.GetTime(),
			MethodId:           method,
			From:               tx.GetFrom(),
			To:                 to,
			Value:              tx.GetValue(),
			TxFee:              fee,
			GasPrice:           tx.GetGasPrice(),
			IsContractCreation: isContract,
			InvokesContract:    invokesContract,
			ErrorMsg:           tx.GetErrorMsg(),
		}
		// Mark Sender and Recipient for balance update
		markBalanceUpdate(indexedTx.From, []byte{0x0}, bulkMetadataUpdates, cache)
		markBalanceUpdate(indexedTx.To, []byte{0x0}, bulkMetadataUpdates, cache)

		if len(indexedTx.Hash) != 32 {
			logger.Fatalf("retrieved hash of length %v for a tx in block %v", len(indexedTx.Hash), blk.GetNumber())
		}

		b, err := proto.Marshal(indexedTx)
		if err != nil {
			return nil, nil, err
		}

		mut := gcp_bigtable.NewMutation()
		mut.Set(DEFAULT_FAMILY, DATA_COLUMN, gcp_bigtable.Timestamp(0), b)

		bulkData.Keys = append(bulkData.Keys, key)
		bulkData.Muts = append(bulkData.Muts, mut)

		indexes := []string{
			fmt.Sprintf("%s:I:TX:%x:TO:%x:%s:%s", bigtable.chainId, tx.GetFrom(), to, reversePaddedBigtableTimestamp(blk.GetTime()), iReverse),
			fmt.Sprintf("%s:I:TX:%x:TIME:%s:%s", bigtable.chainId, tx.GetFrom(), reversePaddedBigtableTimestamp(blk.GetTime()), iReverse),
			fmt.Sprintf("%s:I:TX:%x:BLOCK:%s:%s", bigtable.chainId, tx.GetFrom(), reversedPaddedBlockNumber(blk.GetNumber()), iReverse),
			fmt.Sprintf("%s:I:TX:%x:METHOD:%x:%s:%s", bigtable.chainId, tx.GetFrom(), method, reversePaddedBigtableTimestamp(blk.GetTime()), iReverse),
			fmt.Sprintf("%s:I:TX:%x:FROM:%x:%s:%s", bigtable.chainId, to, tx.GetFrom(), reversePaddedBigtableTimestamp(blk.GetTime()), iReverse),
			fmt.Sprintf("%s:I:TX:%x:TIME:%s:%s", bigtable.chainId, to, reversePaddedBigtableTimestamp(blk.GetTime()), iReverse),
			fmt.Sprintf("%s:I:TX:%x:BLOCK:%s:%s", bigtable.chainId, to, reversedPaddedBlockNumber(blk.GetNumber()), iReverse),
			fmt.Sprintf("%s:I:TX:%x:METHOD:%x:%s:%s", bigtable.chainId, to, method, reversePaddedBigtableTimestamp(blk.GetTime()), iReverse),
		}

		if indexedTx.ErrorMsg != "" {
			indexes = append(indexes, fmt.Sprintf("%s:I:TX:%x:ERROR:%s:%s", bigtable.chainId, tx.GetFrom(), reversePaddedBigtableTimestamp(blk.GetTime()), iReverse))
			indexes = append(indexes, fmt.Sprintf("%s:I:TX:%x:ERROR:%s:%s", bigtable.chainId, to, reversePaddedBigtableTimestamp(blk.GetTime()), iReverse))
		}

		if indexedTx.IsContractCreation {
			indexes = append(indexes, fmt.Sprintf("%s:I:TX:%x:CONTRACT:%s:%s", bigtable.chainId, tx.GetFrom(), reversePaddedBigtableTimestamp(blk.GetTime()), iReverse))
			indexes = append(indexes, fmt.Sprintf("%s:I:TX:%x:CONTRACT:%s:%s", bigtable.chainId, to, reversePaddedBigtableTimestamp(blk.GetTime()), iReverse))
		}

		for _, idx := range indexes {
			mut := gcp_bigtable.NewMutation()
			mut.Set(DEFAULT_FAMILY, key, gcp_bigtable.Timestamp(0), nil)

			bulkData.Keys = append(bulkData.Keys, idx)
			bulkData.Muts = append(bulkData.Muts, mut)
		}

	}

	return bulkData, bulkMetadataUpdates, nil
}

// TransformItx extracts internal transactions from bigtable more specifically from the table blocks.
// It transforms the internal transactions contained within a block and strips any information that is not necessary for our frontend views
// It writes internal transactions to table data:
// Row:    <chainID>:ITX:<TX_HASH>:<paddedITXIndex>
// Family: f
// Column: data
// Cell:   Proto<Eth1InternalTransactionIndexed>
//
// It indexes internal transactions by:
// Row:    <chainID>:I:ITX:<FROM_ADDRESS>:TIME:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<paddedITXIndex>
// Family: f
// Column: <chainID>:ITX:<HASH>:<paddedITXIndex>
// Cell:   nil
// Row:    <chainID>:I:ITX:<TO_ADDRESS>:TIME:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<paddedITXIndex>
// Family: f
// Column: <chainID>:ITX:<HASH>:<paddedITXIndex>
// Cell:   nil
// Row:    <chainID>:I:ITX:<FROM_ADDRESS>:TO:<TO_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<paddedITXIndex>
// Family: f
// Column: <chainID>:ITX:<HASH>:<paddedITXIndex>
// Cell:   nil
// Row:    <chainID>:I:ITX:<TO_ADDRESS>:FROM:<FROM_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<paddedITXIndex>
// Family: f
// Column: <chainID>:ITX:<HASH>:<paddedITXIndex>
// Cell:   nil
func (bigtable *Bigtable) TransformItx(blk *types.Eth1Block, cache *ccache.Cache) (bulkData *types.BulkMutations, bulkMetadataUpdates *types.BulkMutations, err error) {
	bulkData = &types.BulkMutations{}
	bulkMetadataUpdates = &types.BulkMutations{}

	for i, tx := range blk.GetTransactions() {
		if i > 9999 {
			return nil, nil, fmt.Errorf("unexpected number of transactions in block expected at most 9999 but got: %v, tx: %x", i, tx.GetHash())
		}
		iReversed := reversePaddedIndex(i, 10000)

		for j, idx := range tx.GetItx() {
			if j > 999999 {
				return nil, nil, fmt.Errorf("unexpected number of internal transactions in block expected at most 999999 but got: %v, tx: %x", j, tx.GetHash())
			}
			jReversed := reversePaddedIndex(j, 100000)

			if idx.Path == "[]" || bytes.Equal(idx.Value, []byte{0x0}) { // skip top level call & empty calls
				continue
			}

			key := fmt.Sprintf("%s:ITX:%x:%s", bigtable.chainId, tx.GetHash(), jReversed)
			indexedItx := &types.Eth1InternalTransactionIndexed{
				ParentHash:  tx.GetHash(),
				BlockNumber: blk.GetNumber(),
				Time:        blk.GetTime(),
				Type:        idx.GetType(),
				From:        idx.GetFrom(),
				To:          idx.GetTo(),
				Value:       idx.GetValue(),
			}

			markBalanceUpdate(indexedItx.To, []byte{0x0}, bulkMetadataUpdates, cache)
			markBalanceUpdate(indexedItx.From, []byte{0x0}, bulkMetadataUpdates, cache)

			b, err := proto.Marshal(indexedItx)
			if err != nil {
				return nil, nil, err
			}

			mut := gcp_bigtable.NewMutation()
			mut.Set(DEFAULT_FAMILY, DATA_COLUMN, gcp_bigtable.Timestamp(0), b)

			bulkData.Keys = append(bulkData.Keys, key)
			bulkData.Muts = append(bulkData.Muts, mut)

			indexes := []string{
				// fmt.Sprintf("%s:i:ITX::%s:%s:%s", bigtable.chainId, reversePaddedBigtableTimestamp(blk.GetTime()), fmt.Sprintf("%04d", i), fmt.Sprintf("%05d", j)),
				fmt.Sprintf("%s:I:ITX:%x:TO:%x:%s:%s:%s", bigtable.chainId, idx.GetFrom(), idx.GetTo(), reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ITX:%x:FROM:%x:%s:%s:%s", bigtable.chainId, idx.GetTo(), idx.GetFrom(), reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ITX:%x:TIME:%s:%s:%s", bigtable.chainId, idx.GetFrom(), reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ITX:%x:TIME:%s:%s:%s", bigtable.chainId, idx.GetTo(), reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
			}

			for _, idx := range indexes {
				mut := gcp_bigtable.NewMutation()
				mut.Set(DEFAULT_FAMILY, key, gcp_bigtable.Timestamp(0), nil)

				bulkData.Keys = append(bulkData.Keys, idx)
				bulkData.Muts = append(bulkData.Muts, mut)
			}
		}
	}

	return bulkData, bulkMetadataUpdates, nil
}

// https://etherscan.io/tx/0xb10588bde42cb8eb14e72d24088bd71ad3903857d23d50b3ba4187c0cb7d3646#eventlog
// TransformERC20 accepts an eth1 block and creates bigtable mutations for ERC20 transfer events.
// It transforms the logs contained within a block and writes the transformed logs to bigtable
// It writes ERC20 events to the table data:
// Row:    <chainID>:ERC20:<txHash>:<paddedLogIndex>
// Family: f
// Column: data
// Cell:   Proto<Eth1ERC20Indexed>
// Example scan: "1:ERC20:b10588bde42cb8eb14e72d24088bd71ad3903857d23d50b3ba4187c0cb7d3646" returns mainnet ERC20 event(s) for transaction 0xb10588bde42cb8eb14e72d24088bd71ad3903857d23d50b3ba4187c0cb7d3646
//
// It indexes ERC20 events by:
// Row:    <chainID>:I:ERC20:<TOKEN_ADDRESS>:TIME:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC20:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC20:<FROM_ADDRESS>:TIME:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC20:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC20:<TO_ADDRESS>:TIME:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC20:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC20:<FROM_ADDRESS>:TO:<TO_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC20:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC20:<TO_ADDRESS>:FROM:<FROM_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC20:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC20:<FROM_ADDRESS>:TOKEN_SENT:<TOKEN_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC20:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC20:<TO_ADDRESS>:TOKEN_RECEIVED:<TOKEN_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC20:<txHash>:<paddedLogIndex>
// Cell:   nil
func (bigtable *Bigtable) TransformERC20(blk *types.Eth1Block, cache *ccache.Cache) (bulkData *types.BulkMutations, bulkMetadataUpdates *types.BulkMutations, err error) {
	bulkData = &types.BulkMutations{}
	bulkMetadataUpdates = &types.BulkMutations{}

	filterer, err := erc20.NewErc20Filterer(common.Address{}, nil)
	if err != nil {
		log.Printf("error creating filterer: %v", err)
	}

	for i, tx := range blk.GetTransactions() {
		if i > 9999 {
			return nil, nil, fmt.Errorf("unexpected number of transactions in block expected at most 9999 but got: %v, tx: %x", i, tx.GetHash())
		}
		iReversed := reversePaddedIndex(i, 10000)
		for j, log := range tx.GetLogs() {
			if j > 99999 {
				return nil, nil, fmt.Errorf("unexpected number of logs in block expected at most 99999 but got: %v tx: %x", j, tx.GetHash())
			}
			jReversed := reversePaddedIndex(j, 100000)
			if len(log.GetTopics()) != 3 || !bytes.Equal(log.GetTopics()[0], erc20.TransferTopic) {
				continue
			}

			topics := make([]common.Hash, 0, len(log.GetTopics()))

			for _, lTopic := range log.GetTopics() {
				topics = append(topics, common.BytesToHash(lTopic))
			}

			ethLog := eth_types.Log{
				Address:     common.BytesToAddress(log.GetAddress()),
				Data:        log.Data,
				Topics:      topics,
				BlockNumber: blk.GetNumber(),
				TxHash:      common.BytesToHash(tx.GetHash()),
				TxIndex:     uint(i),
				BlockHash:   common.BytesToHash(blk.GetHash()),
				Index:       uint(j),
				Removed:     log.GetRemoved(),
			}

			transfer, _ := filterer.ParseTransfer(ethLog)
			if transfer == nil {
				continue
			}

			value := []byte{}
			if transfer != nil && transfer.Value != nil {
				value = transfer.Value.Bytes()
			}

			key := fmt.Sprintf("%s:ERC20:%x:%s", bigtable.chainId, tx.GetHash(), jReversed)
			indexedLog := &types.Eth1ERC20Indexed{
				ParentHash:   tx.GetHash(),
				BlockNumber:  blk.GetNumber(),
				Time:         blk.GetTime(),
				TokenAddress: log.Address,
				From:         transfer.From.Bytes(),
				To:           transfer.To.Bytes(),
				Value:        value,
			}
			markBalanceUpdate(indexedLog.From, indexedLog.TokenAddress, bulkMetadataUpdates, cache)
			markBalanceUpdate(indexedLog.To, indexedLog.TokenAddress, bulkMetadataUpdates, cache)

			b, err := proto.Marshal(indexedLog)
			if err != nil {
				return nil, nil, err
			}

			mut := gcp_bigtable.NewMutation()
			mut.Set(DEFAULT_FAMILY, DATA_COLUMN, gcp_bigtable.Timestamp(0), b)

			bulkData.Keys = append(bulkData.Keys, key)
			bulkData.Muts = append(bulkData.Muts, mut)

			indexes := []string{
				fmt.Sprintf("%s:I:ERC20:%x:TIME:%s:%s:%s", bigtable.chainId, indexedLog.TokenAddress, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC20:%x:TIME:%s:%s:%s", bigtable.chainId, indexedLog.From, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC20:%x:TO:%x:%s:%s:%s", bigtable.chainId, indexedLog.From, indexedLog.To, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC20:%x:TIME:%s:%s:%s", bigtable.chainId, indexedLog.To, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC20:%x:FROM:%x:%s:%s:%s", bigtable.chainId, indexedLog.To, indexedLog.From, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC20:%x:TOKEN_SENT:%x:%s:%s:%s", bigtable.chainId, indexedLog.From, indexedLog.TokenAddress, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC20:%x:TOKEN_RECEIVED:%x:%s:%s:%s", bigtable.chainId, indexedLog.To, indexedLog.TokenAddress, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
			}

			for _, idx := range indexes {
				mut := gcp_bigtable.NewMutation()
				mut.Set(DEFAULT_FAMILY, key, gcp_bigtable.Timestamp(0), nil)

				bulkData.Keys = append(bulkData.Keys, idx)
				bulkData.Muts = append(bulkData.Muts, mut)
			}
		}
	}

	return bulkData, bulkMetadataUpdates, nil
}

// example: https://etherscan.io/tx/0x4d3a6c56cecb40637c070601c275df9cc7b599b5dc1d5ac2473c92c7a9e62c64#eventlog
// TransformERC721 accepts an eth1 block and creates bigtable mutations for erc721 transfer events.
// It transforms the logs contained within a block and writes the transformed logs to bigtable
// It writes erc721 events to the table data:
// Row:    <chainID>:ERC721:<txHash>:<paddedLogIndex>
// Family: f
// Column: data
// Cell:   Proto<Eth1ERC721Indexed>
// Example scan: "1:ERC721:4d3a6c56cecb40637c070601c275df9cc7b599b5dc1d5ac2473c92c7a9e62c64" returns mainnet ERC721 event(s) for transaction 0x4d3a6c56cecb40637c070601c275df9cc7b599b5dc1d5ac2473c92c7a9e62c64
//
// It indexes ERC721 events by:
// Row:    <chainID>:I:ERC721:<FROM_ADDRESS>:TIME:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC721:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC721:<TO_ADDRESS>:TIME:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC721:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC721:<TOKEN_ADDRESS>:TIME:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC721:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC721:<FROM_ADDRESS>:TO:<TO_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC721:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC721:<TO_ADDRESS>:FROM:<FROM_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC721:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC721:<FROM_ADDRESS>:TOKEN_SENT:<TOKEN_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC721:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC721:<TO_ADDRESS>:TOKEN_RECEIVED:<TOKEN_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC721:<txHash>:<paddedLogIndex>
// Cell:   nil
func (bigtable *Bigtable) TransformERC721(blk *types.Eth1Block, cache *ccache.Cache) (bulkData *types.BulkMutations, bulkMetadataUpdates *types.BulkMutations, err error) {
	bulkData = &types.BulkMutations{}
	bulkMetadataUpdates = &types.BulkMutations{}

	filterer, err := erc721.NewErc721Filterer(common.Address{}, nil)
	if err != nil {
		log.Printf("error creating filterer: %v", err)
	}

	for i, tx := range blk.GetTransactions() {
		if i > 9999 {
			return nil, nil, fmt.Errorf("unexpected number of transactions in block expected at most 9999 but got: %v, tx: %x", i, tx.GetHash())
		}
		iReversed := reversePaddedIndex(i, 10000)
		for j, log := range tx.GetLogs() {
			if j > 99999 {
				return nil, nil, fmt.Errorf("unexpected number of logs in block expected at most 99999 but got: %v tx: %x", j, tx.GetHash())
			}
			if len(log.GetTopics()) != 4 || !bytes.Equal(log.GetTopics()[0], erc721.TransferTopic) {
				continue
			}
			jReversed := reversePaddedIndex(j, 100000)

			topics := make([]common.Hash, 0, len(log.GetTopics()))

			for _, lTopic := range log.GetTopics() {
				topics = append(topics, common.BytesToHash(lTopic))
			}

			ethLog := eth_types.Log{
				Address:     common.BytesToAddress(log.GetAddress()),
				Data:        log.Data,
				Topics:      topics,
				BlockNumber: blk.GetNumber(),
				TxHash:      common.BytesToHash(tx.GetHash()),
				TxIndex:     uint(i),
				BlockHash:   common.BytesToHash(blk.GetHash()),
				Index:       uint(j),
				Removed:     log.GetRemoved(),
			}

			transfer, _ := filterer.ParseTransfer(ethLog)
			if transfer == nil {
				continue
			}

			tokenId := new(big.Int)
			if transfer != nil && transfer.TokenId != nil {
				tokenId = transfer.TokenId
			}

			key := fmt.Sprintf("%s:ERC721:%x:%s", bigtable.chainId, tx.GetHash(), jReversed)
			indexedLog := &types.Eth1ERC721Indexed{
				ParentHash:   tx.GetHash(),
				BlockNumber:  blk.GetNumber(),
				Time:         blk.GetTime(),
				TokenAddress: log.Address,
				From:         transfer.From.Bytes(),
				To:           transfer.To.Bytes(),
				TokenId:      tokenId.Bytes(),
			}

			b, err := proto.Marshal(indexedLog)
			if err != nil {
				return nil, nil, err
			}

			mut := gcp_bigtable.NewMutation()
			mut.Set(DEFAULT_FAMILY, DATA_COLUMN, gcp_bigtable.Timestamp(0), b)

			bulkData.Keys = append(bulkData.Keys, key)
			bulkData.Muts = append(bulkData.Muts, mut)

			indexes := []string{
				// fmt.Sprintf("%s:I:ERC721:%s:%s:%s", bigtable.chainId, reversePaddedBigtableTimestamp(blk.GetTime()), fmt.Sprintf("%04d", i), fmt.Sprintf("%05d", j)),
				fmt.Sprintf("%s:I:ERC721:%x:TIME:%s:%s:%s", bigtable.chainId, indexedLog.TokenAddress, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC721:%x:TIME:%s:%s:%s", bigtable.chainId, indexedLog.From, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC721:%x:TO:%x:%s:%s:%s", bigtable.chainId, indexedLog.From, indexedLog.To, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC721:%x:TIME:%s:%s:%s", bigtable.chainId, indexedLog.To, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC721:%x:FROM:%x:%s:%s:%s", bigtable.chainId, indexedLog.To, indexedLog.From, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC721:%x:TOKEN_SENT:%x:%s:%s:%s", bigtable.chainId, indexedLog.From, indexedLog.TokenAddress, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC721:%x:TOKEN_RECEIVED:%x:%s:%s:%s", bigtable.chainId, indexedLog.To, indexedLog.TokenAddress, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
			}

			for _, idx := range indexes {
				mut := gcp_bigtable.NewMutation()
				mut.Set(DEFAULT_FAMILY, key, gcp_bigtable.Timestamp(0), nil)

				bulkData.Keys = append(bulkData.Keys, idx)
				bulkData.Muts = append(bulkData.Muts, mut)
			}
		}
	}

	return bulkData, bulkMetadataUpdates, nil
}

// TransformERC1155 accepts an eth1 block and creates bigtable mutations for erc1155 transfer events.
// Example: https://etherscan.io/tx/0xcffdd4b44ba9361a769a559c360293333d09efffeab79c36125bb4b20bd04270#eventlog
// It transforms the logs contained within a block and writes the transformed logs to bigtable
// It writes erc1155 events to the table data:
// Row:    <chainID>:ERC1155:<txHash>:<paddedLogIndex>
// Family: f
// Column: data
// Cell:   Proto<Eth1ERC1155Indexed>
// Example scan: "1:ERC1155:cffdd4b44ba9361a769a559c360293333d09efffeab79c36125bb4b20bd04270" returns mainnet erc1155 event(s) for transaction 0xcffdd4b44ba9361a769a559c360293333d09efffeab79c36125bb4b20bd04270
//
// It indexes erc1155 events by:
// Row:    <chainID>:I:ERC1155:<FROM_ADDRESS>:TIME:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC1155:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC1155:<TO_ADDRESS>:TIME:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC1155:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC1155:<TOKEN_ADDRESS>:TIME:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC1155:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC1155:<TO_ADDRESS>:TO:<FROM_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC1155:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC1155:<FROM_ADDRESS>:FROM:<TO_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC1155:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC1155:<FROM_ADDRESS>:TOKEN_SENT:<TOKEN_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC1155:<txHash>:<paddedLogIndex>
// Cell:   nil
//
// Row:    <chainID>:I:ERC1155:<TO_ADDRESS>:TOKEN_RECEIVED:<TOKEN_ADDRESS>:<reversePaddedBigtableTimestamp>:<paddedTxIndex>:<PaddedLogIndex>
// Family: f
// Column: <chainID>:ERC1155:<txHash>:<paddedLogIndex>
// Cell:   nil
func (bigtable *Bigtable) TransformERC1155(blk *types.Eth1Block, cache *ccache.Cache) (bulkData *types.BulkMutations, bulkMetadataUpdates *types.BulkMutations, err error) {
	bulkData = &types.BulkMutations{}
	bulkMetadataUpdates = &types.BulkMutations{}

	filterer, err := erc1155.NewErc1155Filterer(common.Address{}, nil)
	if err != nil {
		log.Printf("error creating filterer: %v", err)
	}

	for i, tx := range blk.GetTransactions() {
		if i > 9999 {
			return nil, nil, fmt.Errorf("unexpected number of transactions in block expected at most 9999 but got: %v, tx: %x", i, tx.GetHash())
		}
		iReversed := reversePaddedIndex(i, 10000)
		for j, log := range tx.GetLogs() {
			if j > 99999 {
				return nil, nil, fmt.Errorf("unexpected number of logs in block expected at most 99999 but got: %v tx: %x", j, tx.GetHash())
			}
			jReversed := reversePaddedIndex(j, 100000)

			key := fmt.Sprintf("%s:ERC1155:%x:%s", bigtable.chainId, tx.GetHash(), jReversed)

			// no events emitted continue
			if len(log.GetTopics()) != 4 || (!bytes.Equal(log.GetTopics()[0], erc1155.TransferBulkTopic) && !bytes.Equal(log.GetTopics()[0], erc1155.TransferSingleTopic)) {
				continue
			}

			topics := make([]common.Hash, 0, len(log.GetTopics()))

			for _, lTopic := range log.GetTopics() {
				topics = append(topics, common.BytesToHash(lTopic))
			}

			ethLog := eth_types.Log{
				Address:     common.BytesToAddress(log.GetAddress()),
				Data:        log.Data,
				Topics:      topics,
				BlockNumber: blk.GetNumber(),
				TxHash:      common.BytesToHash(tx.GetHash()),
				TxIndex:     uint(i),
				BlockHash:   common.BytesToHash(blk.GetHash()),
				Index:       uint(j),
				Removed:     log.GetRemoved(),
			}

			indexedLog := &types.ETh1ERC1155Indexed{}
			transferBatch, _ := filterer.ParseTransferBatch(ethLog)
			transferSingle, _ := filterer.ParseTransferSingle(ethLog)
			if transferBatch == nil && transferSingle == nil {
				continue
			}

			// && len(transferBatch.Operator) == 20 && len(transferBatch.From) == 20 && len(transferBatch.To) == 20 && len(transferBatch.Ids) > 0 && len(transferBatch.Values) > 0
			if transferBatch != nil {
				ids := make([][]byte, 0, len(transferBatch.Ids))
				for _, id := range transferBatch.Ids {
					ids = append(ids, id.Bytes())
				}

				values := make([][]byte, 0, len(transferBatch.Values))
				for _, val := range transferBatch.Values {
					values = append(values, val.Bytes())
				}

				if len(ids) != len(values) {
					logrus.Errorf("error parsing erc1155 batch transfer logs. Expected len(ids): %v len(values): %v to be the same", len(ids), len(values))
					continue
				}
				for ti := range ids {
					indexedLog.BlockNumber = blk.GetNumber()
					indexedLog.Time = blk.GetTime()
					indexedLog.ParentHash = tx.GetHash()
					indexedLog.From = transferBatch.From.Bytes()
					indexedLog.To = transferBatch.To.Bytes()
					indexedLog.Operator = transferBatch.Operator.Bytes()
					indexedLog.TokenId = ids[ti]
					indexedLog.Value = values[ti]
					indexedLog.TokenAddress = log.GetAddress()
				}
			} else if transferSingle != nil {
				indexedLog.BlockNumber = blk.GetNumber()
				indexedLog.Time = blk.GetTime()
				indexedLog.ParentHash = tx.GetHash()
				indexedLog.From = transferSingle.From.Bytes()
				indexedLog.To = transferSingle.To.Bytes()
				indexedLog.Operator = transferSingle.Operator.Bytes()
				indexedLog.TokenId = transferSingle.Id.Bytes()
				indexedLog.Value = transferSingle.Value.Bytes()
				indexedLog.TokenAddress = log.GetAddress()
			}

			b, err := proto.Marshal(indexedLog)
			if err != nil {
				return nil, nil, err
			}

			mut := gcp_bigtable.NewMutation()
			mut.Set(DEFAULT_FAMILY, DATA_COLUMN, gcp_bigtable.Timestamp(0), b)

			bulkData.Keys = append(bulkData.Keys, key)
			bulkData.Muts = append(bulkData.Muts, mut)

			indexes := []string{
				// fmt.Sprintf("%s:I:ERC1155:%s:%s:%s", bigtable.chainId, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC1155:%x:TIME:%s:%s:%s", bigtable.chainId, indexedLog.TokenAddress, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC1155:%x:TIME:%s:%s:%s", bigtable.chainId, indexedLog.From, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC1155:%x:TO:%x:%s:%s:%s", bigtable.chainId, indexedLog.From, indexedLog.To, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC1155:%x:TIME:%s:%s:%s", bigtable.chainId, indexedLog.To, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC1155:%x:FROM:%x:%s:%s:%s", bigtable.chainId, indexedLog.To, indexedLog.From, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC1155:%x:TOKEN_SENT:%x:%s:%s:%s", bigtable.chainId, indexedLog.From, indexedLog.TokenAddress, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
				fmt.Sprintf("%s:I:ERC1155:%x:TOKEN_RECEIVED:%x:%s:%s:%s", bigtable.chainId, indexedLog.To, indexedLog.TokenAddress, reversePaddedBigtableTimestamp(blk.GetTime()), iReversed, jReversed),
			}

			for _, idx := range indexes {
				mut := gcp_bigtable.NewMutation()
				mut.Set(DEFAULT_FAMILY, key, gcp_bigtable.Timestamp(0), nil)

				bulkData.Keys = append(bulkData.Keys, idx)
				bulkData.Muts = append(bulkData.Muts, mut)
			}
		}
	}

	return bulkData, bulkMetadataUpdates, nil
}

// TransformUncle accepts an eth1 block and creates bigtable mutations.
// It transforms the uncles contained within a block, extracts the necessary information to create a view and writes that information to bigtable
// It writes uncles to table data:
// Row:    <chainID>:U:<reversePaddedNumber>
// Family: f
// Column: data
// Cell:   Proto<Eth1UncleIndexed>
// Example scan: "1:U:" returns mainnet uncles mined in desc order
// Example scan: "1:U:984886725" returns mainnet uncles mined after block 15113275 (1000000000 - 984886725)
//
// It indexes uncles by:
// Row:    <chainID>:I:U:<Miner>:TIME:<reversePaddedBigtableTimestamp>
// Family: f
// Column: <chainID>:U:<reversePaddedNumber>
// Cell:   nil
// Example lookup: "1:I:U:ea674fdde714fd979de3edf0f56aa9716b898ec8:TIME:" returns mainnet uncles mined by ethermine in desc order
func (bigtable *Bigtable) TransformUncle(block *types.Eth1Block, cache *ccache.Cache) (bulkData *types.BulkMutations, bulkMetadataUpdates *types.BulkMutations, err error) {
	bulkData = &types.BulkMutations{}
	bulkMetadataUpdates = &types.BulkMutations{}

	for i, uncle := range block.Uncles {
		if i > 99 {
			return nil, nil, fmt.Errorf("unexpected number of uncles in block expected at most 99 but got: %v", i)
		}
		iReversed := reversePaddedIndex(i, 10)
		r := new(big.Int)

		r.Add(big.NewInt(int64(uncle.GetNumber())), big.NewInt(8))
		r.Sub(r, big.NewInt(int64(block.GetNumber())))
		r.Mul(r, utils.BlockReward(block.GetNumber()))
		r.Div(r, big.NewInt(8))

		r.Div(utils.BlockReward(block.GetNumber()), big.NewInt(32))

		uncleIndexed := types.Eth1UncleIndexed{
			Number:      uncle.GetNumber(),
			BlockNumber: block.GetNumber(),
			GasLimit:    uncle.GetGasLimit(),
			GasUsed:     uncle.GetGasUsed(),
			BaseFee:     uncle.GetBaseFee(),
			Difficulty:  uncle.GetDifficulty(),
			Time:        uncle.GetTime(),
			Reward:      r.Bytes(),
		}
		// store uncles in with the key <chainid>:U:<reversePaddedBlockNumber>:<reversePaddedUncleIndex>
		key := fmt.Sprintf("%s:U:%s:%s", bigtable.chainId, reversedPaddedBlockNumber(block.GetNumber()), iReversed)
		mut := gcp_bigtable.NewMutation()

		b, err := proto.Marshal(&uncleIndexed)
		if err != nil {
			return nil, nil, fmt.Errorf("error marshalling proto object err: %w", err)
		}

		mut.Set(DEFAULT_FAMILY, DATA_COLUMN, gcp_bigtable.Timestamp(0), b)

		bulkData.Keys = append(bulkData.Keys, key)
		bulkData.Muts = append(bulkData.Muts, mut)

		indexes := []string{
			// Index uncle by the miners address
			fmt.Sprintf("%s:I:U:%x:TIME:%s:%s", bigtable.chainId, uncle.GetCoinbase(), reversePaddedBigtableTimestamp(block.Time), iReversed),
		}

		for _, idx := range indexes {
			mut := gcp_bigtable.NewMutation()
			mut.Set(DEFAULT_FAMILY, key, gcp_bigtable.Timestamp(0), nil)

			bulkData.Keys = append(bulkData.Keys, idx)
			bulkData.Muts = append(bulkData.Muts, mut)
		}
	}

	return bulkData, bulkMetadataUpdates, nil
}

func (bigtable *Bigtable) GetEth1TxForAddress(prefix string, limit int64) ([]*types.Eth1TransactionIndexed, string, error) {
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	logger.Infof("searching for prefix %v", prefix)
	// add \x00 to the row range such that we skip the previous value
	rowRange := gcp_bigtable.NewRange(prefix+"\x00", prefixSuccessor(prefix, 5))
	// rowRange := gcp_bigtable.PrefixRange(prefix)
	// logger.Infof("querying for prefix: %v", prefix)
	data := make([]*types.Eth1TransactionIndexed, 0, limit)
	keys := make([]string, 0, limit)
	indexes := make([]string, 0, limit)
	keysMap := make(map[string]*types.Eth1TransactionIndexed, limit)

	err := bigtable.tableData.ReadRows(ctx, rowRange, func(row gcp_bigtable.Row) bool {
		keys = append(keys, strings.TrimPrefix(row[DEFAULT_FAMILY][0].Column, "f:"))
		indexes = append(indexes, row.Key())
		return true
	}, gcp_bigtable.LimitRows(limit))
	if err != nil {
		return nil, "", err
	}

	logger.Infof("found transactions: %v results", len(keys))

	if len(keys) == 0 {
		return data, "", nil
	}

	bigtable.tableData.ReadRows(ctx, gcp_bigtable.RowList(keys), func(row gcp_bigtable.Row) bool {
		b := &types.Eth1TransactionIndexed{}
		err := proto.Unmarshal(row[DEFAULT_FAMILY][0].Value, b)

		if err != nil {
			logrus.Fatal(err)
		}
		keysMap[row.Key()] = b

		return true
	})
	// logger.Infof("adding keys: %+v", keys)
	// logger.Infof("adding indexes: %+v", indexes)
	for _, key := range keys {
		data = append(data, keysMap[key])
	}

	// logger.Infof("returning data len: %v lastkey: %v", len(data), lastKey)

	return data, indexes[len(indexes)-1], nil
}

func (bigtable *Bigtable) GetAddressTransactionsTableData(address string, search string, pageToken string) (*types.DataTableResponse, error) {
	if pageToken == "" {
		pageToken = fmt.Sprintf("%s:I:TX:%s:%s:", bigtable.chainId, address, FILTER_TIME)
	}

	logger.Info(pageToken)

	transactions, lastKey, err := BigtableClient.GetEth1TxForAddress(pageToken, 25)
	if err != nil {
		return nil, err
	}

	tableData := make([][]interface{}, len(transactions))
	for i, t := range transactions {
		from := utils.AddCopyButton(utils.FormatHash(t.From), hex.EncodeToString(t.From))

		if fmt.Sprintf("%x", t.From) != address {
			from = utils.FormatAddressAsLink(t.From, "", false, false)
		}
		to := utils.AddCopyButton(utils.FormatHash(t.To), hex.EncodeToString(t.To))
		if fmt.Sprintf("%x", t.To) != address {
			to = utils.FormatAddressAsLink(t.To, "", false, false)
		}
		method := "Transfer"
		if len(t.MethodId) > 0 {

			if t.InvokesContract {
				method = fmt.Sprintf("0x%x", t.MethodId)
			} else {
				method = "Transfer*"
			}
		}
		// logger.Infof("hash: %x amount: %s", t.Hash, new(big.Int).SetBytes(t.Value))

		tableData[i] = []interface{}{
			utils.FormatTransactionHash(t.Hash),
			utils.FormatMethod(method),
			utils.FormatTimeFromNow(t.Time.AsTime()),
			utils.FormatBlockNumber(t.BlockNumber),
			from,
			to,
			utils.FormatAmount(new(big.Int).SetBytes(t.Value), "ETH", 6),
		}
	}

	data := &types.DataTableResponse{
		// Draw: draw,
		// RecordsTotal:    ,
		// RecordsFiltered: ,
		Data:        tableData,
		PagingToken: lastKey,
	}

	return data, nil
}

func (bigtable *Bigtable) GetEth1BlocksForAddress(prefix string, limit int64) ([]*types.Eth1BlockIndexed, string, error) {
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	// add \x00 to the row range such that we skip the previous value
	rowRange := gcp_bigtable.NewRange(prefix+"\x00", prefixSuccessor(prefix, 4))
	// rowRange := gcp_bigtable.PrefixRange(prefix)
	// logger.Infof("querying for prefix: %v", prefix)
	data := make([]*types.Eth1BlockIndexed, 0, limit)
	keys := make([]string, 0, limit)
	indexes := make([]string, 0, limit)
	keysMap := make(map[string]*types.Eth1BlockIndexed, limit)

	err := bigtable.tableData.ReadRows(ctx, rowRange, func(row gcp_bigtable.Row) bool {
		keys = append(keys, strings.TrimPrefix(row[DEFAULT_FAMILY][0].Column, "f:"))
		indexes = append(indexes, row.Key())
		return true
	}, gcp_bigtable.LimitRows(limit))
	if err != nil {
		return nil, "", err
	}

	logger.Infof("found eth1blocks: %v results", len(keys))

	if len(keys) == 0 {
		return data, "", nil
	}

	bigtable.tableData.ReadRows(ctx, gcp_bigtable.RowList(keys), func(row gcp_bigtable.Row) bool {
		b := &types.Eth1BlockIndexed{}
		err := proto.Unmarshal(row[DEFAULT_FAMILY][0].Value, b)

		if err != nil {
			logrus.Fatal(err)
		}
		keysMap[row.Key()] = b

		return true
	})
	// logger.Infof("adding keys: %+v", keys)
	// logger.Infof("adding indexes: %+v", indexes)
	for _, key := range keys {
		data = append(data, keysMap[key])
	}

	// logger.Infof("returning data len: %v lastkey: %v", len(data), lastKey)

	return data, indexes[len(indexes)-1], nil
}

func (bigtable *Bigtable) GetAddressBlocksMinedTableData(address string, search string, pageToken string) (*types.DataTableResponse, error) {
	if pageToken == "" {
		pageToken = fmt.Sprintf("%s:I:B:%s:", bigtable.chainId, address)
	}

	blocks, lastKey, err := BigtableClient.GetEth1BlocksForAddress(pageToken, 25)
	if err != nil {
		return nil, err
	}

	tableData := make([][]interface{}, len(blocks))
	for i, b := range blocks {
		// logger.Infof("hash: %x amount: %s", t.Hash, new(big.Int).SetBytes(t.Value))

		reward := new(big.Int).Add(utils.Eth1BlockReward(b.Number), new(big.Int).SetBytes(b.TxReward))

		tableData[i] = []interface{}{
			utils.FormatTransactionHash(b.Hash),
			utils.FormatBlockNumber(b.Number),
			utils.FormatTimeFromNow(b.Time.AsTime()),
			utils.FormatAmount(reward, "ETH", 6),
		}
	}

	data := &types.DataTableResponse{
		// Draw: draw,
		// RecordsTotal:    ,
		// RecordsFiltered: ,
		Data:        tableData,
		PagingToken: lastKey,
	}

	return data, nil
}

func (bigtable *Bigtable) GetEth1UnclesForAddress(prefix string, limit int64) ([]*types.Eth1UncleIndexed, string, error) {
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	// add \x00 to the row range such that we skip the previous value
	rowRange := gcp_bigtable.NewRange(prefix+"\x00", prefixSuccessor(prefix, 4))
	// rowRange := gcp_bigtable.PrefixRange(prefix)
	// logger.Infof("querying for prefix: %v", prefix)
	data := make([]*types.Eth1UncleIndexed, 0, limit)
	keys := make([]string, 0, limit)
	indexes := make([]string, 0, limit)
	keysMap := make(map[string]*types.Eth1UncleIndexed, limit)

	err := bigtable.tableData.ReadRows(ctx, rowRange, func(row gcp_bigtable.Row) bool {
		keys = append(keys, strings.TrimPrefix(row[DEFAULT_FAMILY][0].Column, "f:"))
		indexes = append(indexes, row.Key())
		return true
	}, gcp_bigtable.LimitRows(limit))
	if err != nil {
		return nil, "", err
	}

	logger.Infof("found uncles: %v results", len(keys))

	if len(keys) == 0 {
		return data, "", nil
	}

	bigtable.tableData.ReadRows(ctx, gcp_bigtable.RowList(keys), func(row gcp_bigtable.Row) bool {
		b := &types.Eth1UncleIndexed{}
		err := proto.Unmarshal(row[DEFAULT_FAMILY][0].Value, b)

		if err != nil {
			logrus.Fatal(err)
		}
		keysMap[row.Key()] = b

		return true
	})
	// logger.Infof("adding keys: %+v", keys)
	// logger.Infof("adding indexes: %+v", indexes)
	for _, key := range keys {
		data = append(data, keysMap[key])
	}

	// logger.Infof("returning data len: %v lastkey: %v", len(data), lastKey)

	return data, indexes[len(indexes)-1], nil
}

func (bigtable *Bigtable) GetAddressUnclesMinedTableData(address string, search string, pageToken string) (*types.DataTableResponse, error) {
	if pageToken == "" {
		pageToken = fmt.Sprintf("%s:I:U:%s:", bigtable.chainId, address)
	}

	logger.Info(pageToken)

	uncles, lastKey, err := BigtableClient.GetEth1UnclesForAddress(pageToken, 25)
	if err != nil {
		return nil, err
	}

	tableData := make([][]interface{}, len(uncles))
	for i, u := range uncles {

		tableData[i] = []interface{}{
			utils.FormatBlockNumber(u.Number),
			utils.FormatTimeFromNow(u.Time.AsTime()),
			utils.FormatDifficulty(new(big.Int).SetBytes(u.Difficulty)),
			utils.FormatAmount(new(big.Int).SetBytes(u.Reward), "ETH", 6),
		}
	}

	data := &types.DataTableResponse{
		// Draw: draw,
		// RecordsTotal:    ,
		// RecordsFiltered: ,
		Data:        tableData,
		PagingToken: lastKey,
	}

	return data, nil
}

func (bigtable *Bigtable) GetEth1ItxForAddress(prefix string, limit int64) ([]*types.Eth1InternalTransactionIndexed, string, error) {
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	// add \x00 to the row range such that we skip the previous value
	rowRange := gcp_bigtable.NewRange(prefix+"\x00", prefixSuccessor(prefix, 5))
	data := make([]*types.Eth1InternalTransactionIndexed, 0, limit)
	keys := make([]string, 0, limit)
	indexes := make([]string, 0, limit)

	keysMap := make(map[string]*types.Eth1InternalTransactionIndexed, limit)
	err := bigtable.tableData.ReadRows(ctx, rowRange, func(row gcp_bigtable.Row) bool {

		keys = append(keys, strings.TrimPrefix(row[DEFAULT_FAMILY][0].Column, "f:"))
		indexes = append(indexes, row.Key())
		return true
	}, gcp_bigtable.LimitRows(limit))
	if err != nil {
		return nil, "", err
	}
	if len(keys) == 0 {
		return data, "", nil
	}

	bigtable.tableData.ReadRows(ctx, gcp_bigtable.RowList(keys), func(row gcp_bigtable.Row) bool {
		b := &types.Eth1InternalTransactionIndexed{}
		err := proto.Unmarshal(row[DEFAULT_FAMILY][0].Value, b)

		if err != nil {
			logrus.Fatal(err)
		}
		keysMap[row.Key()] = b
		return true
	})

	for _, key := range keys {
		data = append(data, keysMap[key])
	}

	return data, indexes[len(indexes)-1], nil
}

func (bigtable *Bigtable) GetAddressInternalTableData(address string, search string, pageToken string) (*types.DataTableResponse, error) {
	// defaults to most recent
	if pageToken == "" {
		pageToken = fmt.Sprintf("%s:I:ITX:%s:%s:", bigtable.chainId, address, FILTER_TIME)
	}

	transactions, lastKey, err := bigtable.GetEth1ItxForAddress(pageToken, 25)
	if err != nil {
		return nil, err
	}

	tableData := make([][]interface{}, len(transactions))
	for i, t := range transactions {
		from := utils.FormatHash(t.From)
		if fmt.Sprintf("%x", t.From) != address {
			from = utils.FormatAddressAsLink(t.From, "", false, false)
		}
		to := utils.FormatHash(t.To)
		if fmt.Sprintf("%x", t.To) != address {
			to = utils.FormatAddressAsLink(t.To, "", false, false)
		}
		tableData[i] = []interface{}{
			utils.FormatTransactionHash(t.ParentHash),
			utils.FormatTimeFromNow(t.Time.AsTime()),
			from,
			to,
			utils.FormatAmount(new(big.Int).SetBytes(t.Value), "ETH", 6),
			t.Type,
		}
	}

	data := &types.DataTableResponse{
		Data:        tableData,
		PagingToken: lastKey,
	}

	return data, nil
}

func (bigtable *Bigtable) GetAddressInternalTransactionsTableData(address string, search string, pageToken string) (*types.DataTableResponse, error) {
	if pageToken == "" {
		pageToken = fmt.Sprintf("%s:I:ITX:%s:%s:", bigtable.chainId, address, FILTER_TIME)
	}

	transactions, lastKey, err := BigtableClient.GetEth1ItxForAddress(pageToken, 25)
	if err != nil {
		return nil, err
	}

	tableData := make([][]interface{}, len(transactions))
	for i, t := range transactions {
		from := utils.FormatHash(t.From)

		if fmt.Sprintf("%x", t.From) != address {
			from = utils.FormatAddressAsLink(t.From, "", false, false)
		}
		to := utils.FormatHash(t.To)
		if fmt.Sprintf("%x", t.To) != address {
			to = utils.FormatAddressAsLink(t.To, "", false, false)
		}
		tableData[i] = []interface{}{
			utils.FormatTransactionHash(t.ParentHash),
			utils.FormatTimeFromNow(t.Time.AsTime()),
			from,
			to,
			utils.FormatAmount(new(big.Int).SetBytes(t.Value), "ETH", 6),
			t.Type,
		}
	}

	data := &types.DataTableResponse{
		// Draw: draw,
		// RecordsTotal:    ,
		// RecordsFiltered: ,
		Data:        tableData,
		PagingToken: lastKey,
	}

	return data, nil
}

func (bigtable *Bigtable) GetAddressERC20TransfersTableData(address string, search string, pageToken string) (*types.DataTableResponse, error) {
	if pageToken == "" {
		pageToken = fmt.Sprintf("%s:I:ERC20:%s:%s:", bigtable.chainId, address, FILTER_TIME)
	}

	transactions, lastKey, err := BigtableClient.GetEth1ERC20ForAddress(pageToken, 25)
	if err != nil {
		return nil, err
	}

	tableData := make([][]interface{}, len(transactions))
	for i, t := range transactions {
		from := utils.FormatHash(t.From)

		if fmt.Sprintf("%x", t.From) != address {
			from = utils.FormatAddressAsLink(t.From, "", false, false)
		}
		to := utils.FormatHash(t.To)
		if fmt.Sprintf("%x", t.To) != address {
			to = utils.FormatAddressAsLink(t.To, "", false, false)
		}

		tokenData := erc20.GetTokenDetail(fmt.Sprintf("%x", t.TokenAddress))
		value := new(big.Int).SetBytes(t.Value).String()
		symbol := fmt.Sprintf(" 0x%x...", t.TokenAddress[:8])
		if tokenData != nil {
			value = tokenData.FormatAmount(new(big.Int).SetBytes(t.Value))
			symbol = fmt.Sprintf("<a href='#''>%s (%s)</a>", tokenData.Name, tokenData.Symbol)
		}
		tableData[i] = []interface{}{
			utils.FormatTransactionHash(t.ParentHash),
			utils.FormatTimeFromNow(t.Time.AsTime()),
			from,
			to,
			value,
			symbol,
		}
	}

	data := &types.DataTableResponse{
		// Draw: draw,
		// RecordsTotal:    ,
		// RecordsFiltered: ,
		Data:        tableData,
		PagingToken: lastKey,
	}

	return data, nil
}

func (bigtable *Bigtable) GetEth1ERC20ForAddress(prefix string, limit int64) ([]*types.Eth1ERC20Indexed, string, error) {
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	// add \x00 to the row range such that we skip the previous value
	rowRange := gcp_bigtable.NewRange(prefix+"\x00", prefixSuccessor(prefix, 5))
	data := make([]*types.Eth1ERC20Indexed, 0, limit)
	keys := make([]string, 0, limit)
	indexes := make([]string, 0, limit)

	keysMap := make(map[string]*types.Eth1ERC20Indexed, limit)
	err := bigtable.tableData.ReadRows(ctx, rowRange, func(row gcp_bigtable.Row) bool {
		keys = append(keys, strings.TrimPrefix(row[DEFAULT_FAMILY][0].Column, "f:"))
		indexes = append(indexes, row.Key())
		return true
	}, gcp_bigtable.LimitRows(limit))
	if err != nil {
		return nil, "", err
	}
	if len(keys) == 0 {
		return data, "", nil
	}

	bigtable.tableData.ReadRows(ctx, gcp_bigtable.RowList(keys), func(row gcp_bigtable.Row) bool {
		b := &types.Eth1ERC20Indexed{}
		err := proto.Unmarshal(row[DEFAULT_FAMILY][0].Value, b)

		if err != nil {
			logrus.Fatal(err)
		}
		keysMap[row.Key()] = b
		return true
	})

	for _, key := range keys {
		data = append(data, keysMap[key])
	}

	return data, indexes[len(indexes)-1], nil
}

func (bigtable *Bigtable) GetAddressErc20TableData(address string, search string, pageToken string) (*types.DataTableResponse, error) {

	if pageToken == "" {
		pageToken = fmt.Sprintf("%s:I:ERC20:%s:%s:", bigtable.chainId, address, FILTER_TIME)
	}

	transactions, lastKey, err := bigtable.GetEth1ERC20ForAddress(pageToken, 25)
	if err != nil {
		return nil, err
	}

	tableData := make([][]interface{}, len(transactions))
	for i, t := range transactions {
		from := utils.FormatHash(t.From)
		if fmt.Sprintf("%x", t.From) != address {
			from = utils.FormatAddressAsLink(t.From, "", false, false)
		}
		to := utils.FormatHash(t.To)
		if fmt.Sprintf("%x", t.To) != address {
			to = utils.FormatAddressAsLink(t.To, "", false, false)
		}
		tableData[i] = []interface{}{
			utils.FormatTransactionHash(t.ParentHash),
			from,
			to,
			new(big.Int).SetBytes(t.Value),
			utils.FormatAddressAsLink(t.TokenAddress, "", false, true),
			// utils.FormatAmount(float64(new(big.Int).SetBytes(t.Value).Int64()), "ETH", 6),
		}
	}

	data := &types.DataTableResponse{
		Data:        tableData,
		PagingToken: lastKey,
	}

	return data, nil
}

func (bigtable *Bigtable) GetEth1ERC20TxForAddressCount(address string, filterKey IndexFilter) (uint64, error) {
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	prefix := fmt.Sprintf("%s:I:ERC20:%s:%s", bigtable.chainId, address, filterKey)

	rowRange := gcp_bigtable.PrefixRange(prefix)

	sum := uint64(0)

	err := bigtable.tableData.ReadRows(ctx, rowRange, func(row gcp_bigtable.Row) bool {
		sum += 1
		return true
	})
	if err != nil {
		return 0, err
	}

	return sum, nil
}

func (bigtable *Bigtable) GetEth1ERC721ForAddress(prefix string, limit int64) ([]*types.Eth1ERC721Indexed, string, error) {
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	// add \x00 to the row range such that we don't include the prefix itself in the response. Converts range to open interval (start, end).
	// "1:I:ERC721:81d98c8fda0410ee3e9d7586cb949cd19fa4cf38:TIME;"
	rowRange := gcp_bigtable.NewRange(prefix+"\x00", prefixSuccessor(prefix, 5))

	data := make([]*types.Eth1ERC721Indexed, 0, limit)

	keys := make([]string, 0, limit)
	keysMap := make(map[string]*types.Eth1ERC721Indexed, limit)
	indexes := make([]string, 0, limit)

	//  1:I:ERC721:81d98c8fda0410ee3e9d7586cb949cd19fa4cf38:TIME:9223372035220135322:0052:00000

	err := bigtable.tableData.ReadRows(ctx, rowRange, func(row gcp_bigtable.Row) bool {
		keys = append(keys, strings.TrimPrefix(row[DEFAULT_FAMILY][0].Column, "f:"))
		indexes = append(indexes, row.Key())
		return true
	}, gcp_bigtable.LimitRows(limit))
	if err != nil {
		return nil, "", err
	}

	if len(keys) == 0 {
		return data, "", nil
	}

	bigtable.tableData.ReadRows(ctx, gcp_bigtable.RowList(keys), func(row gcp_bigtable.Row) bool {
		b := &types.Eth1ERC721Indexed{}
		err := proto.Unmarshal(row[DEFAULT_FAMILY][0].Value, b)

		if err != nil {
			logrus.Fatal(err)
		}
		keysMap[row.Key()] = b
		return true
	})

	for _, key := range keys {
		data = append(data, keysMap[key])
	}
	return data, indexes[len(indexes)-1], nil
}

func (bigtable *Bigtable) GetAddressErc721TableData(address string, search string, pageToken string) (*types.DataTableResponse, error) {

	if pageToken == "" {
		pageToken = fmt.Sprintf("%s:I:ERC721:%s:%s:", bigtable.chainId, address, FILTER_TIME)
		// pageToken = fmt.Sprintf("%s:I:ERC721:%s:%s:9999999999999999999:9999:99999", bigtable.chainId, address, FILTER_TIME)
	}

	transactions, lastKey, err := bigtable.GetEth1ERC721ForAddress(pageToken, 25)
	if err != nil {
		return nil, err
	}

	tableData := make([][]interface{}, len(transactions))
	for i, t := range transactions {
		from := utils.FormatHash(t.From)
		if fmt.Sprintf("%x", t.From) != address {
			from = utils.FormatAddressAsLink(t.From, "", false, false)
		}
		to := utils.FormatHash(t.To)
		if fmt.Sprintf("%x", t.To) != address {
			to = utils.FormatAddressAsLink(t.To, "", false, false)
		}
		tableData[i] = []interface{}{
			utils.FormatTransactionHash(t.ParentHash),
			utils.FormatTimeFromNow(t.Time.AsTime()),
			from,
			to,
			utils.FormatAddressAsLink(t.TokenAddress, "", false, true),
			new(big.Int).SetBytes(t.TokenId).String(),
		}
	}

	data := &types.DataTableResponse{
		Data:        tableData,
		PagingToken: lastKey,
	}

	return data, nil
}

func (bigtable *Bigtable) GetEth1ERC721TxForAddressCount(address string, filterKey IndexFilter) (uint64, error) {
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	prefix := fmt.Sprintf("%s:I:ERC721:%s:%s", bigtable.chainId, address, filterKey)

	rowRange := gcp_bigtable.PrefixRange(prefix)

	sum := uint64(0)

	err := bigtable.tableData.ReadRows(ctx, rowRange, func(row gcp_bigtable.Row) bool {
		sum += 1
		return true
	})
	if err != nil {
		return 0, err
	}

	return sum, nil
}

func (bigtable *Bigtable) GetEth1ERC1155ForAddress(prefix string, limit int64) ([]*types.ETh1ERC1155Indexed, string, error) {
	ctx, cancle := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancle()

	rowRange := gcp_bigtable.NewRange(prefix+"\x00", prefixSuccessor(prefix, 5))

	data := make([]*types.ETh1ERC1155Indexed, 0, limit)

	keys := make([]string, 0, limit)
	keysMap := make(map[string]*types.ETh1ERC1155Indexed, limit)
	indexes := make([]string, 0, limit)

	err := bigtable.tableData.ReadRows(ctx, rowRange, func(row gcp_bigtable.Row) bool {
		keys = append(keys, strings.TrimPrefix(row[DEFAULT_FAMILY][0].Column, "f:"))
		indexes = append(indexes, row.Key())
		return true
	}, gcp_bigtable.LimitRows(limit))
	if err != nil {
		return nil, "", err
	}

	if len(keys) == 0 {
		return data, "", nil
	}

	bigtable.tableData.ReadRows(ctx, gcp_bigtable.RowList(keys), func(row gcp_bigtable.Row) bool {
		b := &types.ETh1ERC1155Indexed{}
		err := proto.Unmarshal(row[DEFAULT_FAMILY][0].Value, b)

		if err != nil {
			logrus.Fatal(err)
		}
		keysMap[row.Key()] = b
		return true
	})

	for _, key := range keys {
		data = append(data, keysMap[key])
	}
	return data, indexes[len(indexes)-1], nil
}

func (bigtable *Bigtable) GetAddressErc1155TableData(address string, search string, pageToken string) (*types.DataTableResponse, error) {
	if pageToken == "" {
		pageToken = fmt.Sprintf("%s:I:ERC1155:%s:%s:", bigtable.chainId, address, FILTER_TIME)
	}

	transactions, lastKey, err := bigtable.GetEth1ERC1155ForAddress(pageToken, 25)
	if err != nil {
		return nil, err
	}

	tableData := make([][]interface{}, len(transactions))
	for i, t := range transactions {
		from := utils.FormatHash(t.From)
		if fmt.Sprintf("%x", t.From) != address {
			from = utils.FormatAddressAsLink(t.From, "", false, false)
		}
		to := utils.FormatHash(t.To)
		if fmt.Sprintf("%x", t.To) != address {
			to = utils.FormatAddressAsLink(t.To, "", false, false)
		}
		tableData[i] = []interface{}{
			utils.FormatTransactionHash(t.ParentHash),
			utils.FormatTimeFromNow(t.Time.AsTime()),
			from,
			to,
			utils.FormatAddressAsLink(t.TokenAddress, "", false, true),
			new(big.Int).SetBytes(t.TokenId).String(),
			new(big.Int).SetBytes(t.Value).String(),
		}
	}

	data := &types.DataTableResponse{
		Data:        tableData,
		PagingToken: lastKey,
	}

	return data, nil
}

func (bigtable *Bigtable) GetEth1ERC1155TxForAddressCount(address string, filterKey IndexFilter) (uint64, error) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancel()

	prefix := fmt.Sprintf("%s:I:ERC1155:%s:%s", bigtable.chainId, address, filterKey)

	rowRange := gcp_bigtable.PrefixRange(prefix)

	sum := uint64(0)

	err := bigtable.tableData.ReadRows(ctx, rowRange, func(row gcp_bigtable.Row) bool {
		sum += 1
		return true
	})
	if err != nil {
		return 0, err
	}

	return sum, nil
}

func (bigtable *Bigtable) GetMetadataUpdates(startToken string, limit int) ([]string, error) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second*30))
	defer cancel()

	res := make([]string, 0, limit)

	err := bigtable.tableMetadataUpdates.ReadRows(ctx, gcp_bigtable.NewRange(startToken, ""), func(row gcp_bigtable.Row) bool {
		res = append(res, row.Key())
		return true
	}, gcp_bigtable.LimitRows(int64(limit)))

	return res, err
}

func prefixSuccessor(prefix string, pos int) string {
	if prefix == "" {
		return "" // infinite range
	}
	split := strings.Split(prefix, ":")
	if len(split) > pos {
		prefix = strings.Join(split[:pos], ":")
	}
	n := len(prefix)
	for n--; n >= 0 && prefix[n] == '\xff'; n-- {
	}
	if n == -1 {
		return ""
	}
	ans := []byte(prefix[:n])
	ans = append(ans, prefix[n]+1)
	return string(ans)
}

func markBalanceUpdate(address []byte, token []byte, mutations *types.BulkMutations, cache *ccache.Cache) {
	balanceUpdateKey := fmt.Sprintf("B:%x:%x", address, token) // format is B: for balance update as prefix + address + token id (0x0 = native ETH token)
	if cache.Get(balanceUpdateKey) == nil {
		mut := gcp_bigtable.NewMutation()
		mut.Set(DEFAULT_FAMILY, DATA_COLUMN, gcp_bigtable.Timestamp(0), []byte{})

		mutations.Keys = append(mutations.Keys, balanceUpdateKey)
		mutations.Muts = append(mutations.Muts, mut)

		cache.Set(balanceUpdateKey, true, time.Hour*48)
	}
}
