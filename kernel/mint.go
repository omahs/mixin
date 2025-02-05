package kernel

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/config"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/kernel/internal/clock"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/dgraph-io/badger/v4"
)

const (
	MainnetMintPeriodForkBatch           = 72
	MainnetMintPeriodForkTimeBegin       = 6
	MainnetMintPeriodForkTimeEnd         = 18
	MainnetMintWorkDistributionForkBatch = 729
	MainnetMintTransactionV2ForkBatch    = 739
	MainnetMintTransactionV3ForkBatch    = 1313
)

var (
	MintPool        common.Integer
	MintLiquidity   common.Integer
	MintYearShares  int
	MintYearBatches int
	MintNodeMaximum int
)

func init() {
	MintPool = common.NewInteger(500000)
	MintLiquidity = common.NewInteger(500000)
	MintYearShares = 10
	MintYearBatches = 365
	MintNodeMaximum = 50
}

func (chain *Chain) AggregateMintWork() {
	logger.Printf("AggregateMintWork(%s)\n", chain.ChainId)
	defer close(chain.wlc)

	round, err := chain.persistStore.ReadWorkOffset(chain.ChainId)
	if err != nil {
		panic(err)
	}
	logger.Printf("AggregateMintWork(%s) begin with %d\n", chain.ChainId, round)

	fork := uint64(SnapshotRoundDayLeapForkHack.UnixNano())
	wait := time.Duration(chain.node.custom.Node.KernelOprationPeriod/2) * time.Second

	for chain.running {
		if cs := chain.State; cs == nil {
			logger.Printf("AggregateMintWork(%s) no state yet\n", chain.ChainId)
			chain.waitOrDone(wait)
			continue
		}
		// FIXME Here continues to update the cache round mostly because no way to
		// decide the last round of a removed node. The fix is to penalize the late
		// spending of a node remove output, i.e. the node remove output must be
		// used as soon as possible.
		// A better fix is to init some transaction that references the node removal
		// all automatically from kernel.
		// Another fix is to utilize the light node to reference the node removal
		// and incentivize the first light nodes that do this.
		crn := chain.State.CacheRound.Number
		if crn < round {
			panic(fmt.Errorf("AggregateMintWork(%s) waiting %d %d", chain.ChainId, crn, round))
		}
		snapshots, err := chain.persistStore.ReadSnapshotWorksForNodeRound(chain.ChainId, round)
		if err != nil {
			logger.Verbosef("AggregateMintWork(%s) ERROR ReadSnapshotsForNodeRound %s\n", chain.ChainId, err.Error())
			continue
		}
		if len(snapshots) == 0 {
			chain.waitOrDone(wait)
			continue
		}
		for chain.running {
			if chain.node.isMainnet() && snapshots[0].Timestamp < fork {
				snapshots = nil
			}
			err = chain.persistStore.WriteRoundWork(chain.ChainId, round, snapshots)
			if err == nil {
				break
			}
			if errors.Is(err, badger.ErrConflict) {
				logger.Verbosef("AggregateMintWork(%s) ERROR WriteRoundWork %s\n", chain.ChainId, err.Error())
				time.Sleep(100 * time.Millisecond)
				continue
			}
			panic(err)
		}
		if round < crn {
			round = round + 1
		} else {
			chain.waitOrDone(wait)
		}
	}

	logger.Printf("AggregateMintWork(%s) end with %d\n", chain.ChainId, round)
}

func (node *Node) MintLoop() {
	defer close(node.mlc)

	ticker := time.NewTicker(time.Duration(node.custom.Node.KernelOprationPeriod) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-node.done:
			return
		case <-ticker.C:
			cur, err := node.persistStore.ReadCustodian(node.GraphTimestamp)
			if err != nil {
				panic(err)
			}
			if cur == nil && node.isMainnet() {
				err := node.tryToMintKernelNodeLegacy()
				logger.Println(node.IdForNetwork, "tryToMintKernelNodeLegacy", err)
			} else {
				err = node.tryToMintUniversal(cur)
				logger.Println(node.IdForNetwork, "tryToMintKernelUniversal", err)
			}
		}
	}
}

func (node *Node) tryToMintUniversal(custodianRequest *common.CustodianUpdateRequest) error {
	signed := node.buildUniversalMintTransaction(custodianRequest, node.GraphTimestamp, false)
	if signed == nil {
		return nil
	}

	err := signed.SignInput(node.persistStore, 0, []*common.Address{&node.Signer})
	if err != nil {
		return err
	}
	err = signed.Validate(node.persistStore, false)
	if err != nil {
		return err
	}
	err = node.persistStore.CachePutTransaction(signed)
	if err != nil {
		return err
	}
	s := &common.Snapshot{
		Version: common.SnapshotVersionCommonEncoding,
		NodeId:  node.IdForNetwork,
	}
	s.AddSoleTransaction(signed.PayloadHash())
	logger.Println("tryToMintUniversal", signed.PayloadHash(), hex.EncodeToString(signed.Marshal()))
	return node.chain.AppendSelfEmpty(s)
}

func (node *Node) buildUniversalMintTransaction(custodianRequest *common.CustodianUpdateRequest, timestamp uint64, validateOnly bool) *common.VersionedTransaction {
	batch, amount := node.checkUniversalMintPossibility(timestamp, validateOnly)
	if amount.Sign() <= 0 || batch <= 0 {
		return nil
	}

	// TODO mint works should calculate according to finalized previous round, new fork required
	kernel := amount.Div(10).Mul(5)
	accepted := node.NodesListWithoutState(timestamp, true)
	mints, err := node.distributeKernelMintByWorks(accepted, kernel, timestamp)
	if err != nil {
		logger.Printf("buildUniversalMintTransaction ERROR %s\n", err.Error())
		return nil
	}

	tx := node.NewTransaction(common.XINAssetId)
	tx.AddUniversalMintInput(uint64(batch), amount)
	total := common.NewInteger(0)
	for _, m := range mints {
		in := fmt.Sprintf("MINTKERNELNODE%d", batch)
		si := crypto.NewHash([]byte(m.Signer.String() + in))
		seed := append(si[:], si[:]...)
		script := common.NewThresholdScript(1)
		tx.AddScriptOutput([]*common.Address{&m.Payee}, script, m.Work, seed)
		total = total.Add(m.Work)
	}
	if total.Cmp(amount) > 0 {
		panic(fmt.Errorf("buildUniversalMintTransaction %s %s", amount, total))
	}

	safe := amount.Div(10).Mul(4)
	domains := node.persistStore.ReadDomains()
	custodian := &domains[0].Account
	if custodianRequest != nil {
		custodian = custodianRequest.Custodian
	}
	in := fmt.Sprintf("MINTCUSTODIANACCOUNT%d", batch)
	si := crypto.NewHash([]byte(custodian.String() + in))
	seed := append(si[:], si[:]...)
	script := common.NewThresholdScript(1)
	tx.AddScriptOutput([]*common.Address{custodian}, script, safe, seed)
	total = total.Add(safe)
	if total.Cmp(amount) > 0 {
		panic(fmt.Errorf("buildUniversalMintTransaction %s %s", amount, total))
	}

	node.tryToSlashLegacyLightPool(uint64(batch), tx)
	amount = tx.Inputs[0].Mint.Amount

	// TODO use real light mint account when light node online
	light := amount.Sub(total)
	addr := common.NewAddressFromSeed(make([]byte, 64))
	script = common.NewThresholdScript(common.Operator64)
	in = fmt.Sprintf("MINTLIGHTACCOUNT%d", batch)
	si = crypto.NewHash([]byte(addr.String() + in))
	seed = append(si[:], si[:]...)
	tx.AddScriptOutput([]*common.Address{&addr}, script, light, seed)
	return tx.AsVersioned()
}

func (node *Node) tryToSlashLegacyLightPool(batch uint64, tx *common.Transaction) {
	if !node.isMainnet() || batch < MainnetMintTransactionV3ForkBatch {
		return
	}
	mint := tx.Inputs[0].Mint
	mints, _, _ := node.persistStore.ReadMintDistributions(batch-1, 1)
	if mints[0].Batch+1 != batch {
		panic(fmt.Errorf("tryToSlashLegacyLightPool %v %d", mints[0], batch))
	}
	if mints[0].Group == mint.Group {
		return
	}
	old := int(mints[0].Batch)
	lightSlash := poolSizeLegacy(old).Sub(poolSizeUniversal(old))
	mint.Amount = mint.Amount.Add(lightSlash)
}

func (node *Node) PoolSize() (common.Integer, error) {
	dist, err := node.persistStore.ReadLastMintDistribution(^uint64(0))
	if err != nil {
		return common.Zero, err
	}
	if dist.Group == "KERNELNODE" {
		return poolSizeLegacy(int(dist.Batch)), nil
	}
	return poolSizeUniversal(int(dist.Batch)), nil
}

func poolSizeUniversal(batch int) common.Integer {
	mint, pool := common.Zero, MintPool
	for i := 0; i < batch/MintYearBatches; i++ {
		year := pool.Div(MintYearShares)
		mint = mint.Add(year)
		pool = pool.Sub(year)
	}
	day := pool.Div(MintYearShares).Div(MintYearBatches)
	if count := batch % MintYearBatches; count > 0 {
		mint = mint.Add(day.Mul(count))
	}
	if mint.Sign() > 0 {
		return MintPool.Sub(mint)
	}
	return MintPool
}

func poolSizeLegacy(batch int) common.Integer {
	mint, pool := common.Zero, MintPool
	for i := 0; i < batch/MintYearBatches; i++ {
		year := pool.Div(MintYearShares)
		mint = mint.Add(year.Div(10).Mul(9))
		pool = pool.Sub(year)
	}
	day := pool.Div(MintYearShares).Div(MintYearBatches)
	if count := batch % MintYearBatches; count > 0 {
		mint = mint.Add(day.Div(10).Mul(9).Mul(count))
	}
	if mint.Sign() > 0 {
		return MintPool.Sub(mint)
	}
	return MintPool
}

func (node *Node) PledgeAmount(ts uint64) common.Integer {
	if ts < node.Epoch {
		return pledgeAmount(0)
	}
	since := uint64(ts) - node.Epoch
	return pledgeAmount(time.Duration(since))
}

func pledgeAmount(sinceEpoch time.Duration) common.Integer {
	batch := int(sinceEpoch / 3600000000000 / 24)
	liquidity, pool := MintLiquidity, MintPool
	for i := 0; i < batch/MintYearBatches; i++ {
		share := pool.Div(MintYearShares)
		liquidity = liquidity.Add(share)
		pool = pool.Sub(share)
	}
	return liquidity.Div(MintNodeMaximum)
}

func (node *Node) buildLegacyKerneNodeMintTransaction(timestamp uint64, validateOnly bool) *common.VersionedTransaction {
	batch, amount := node.checkLegacyMintPossibility(timestamp, validateOnly)
	if amount.Sign() <= 0 || batch <= 0 {
		return nil
	}

	// TODO mint works should calculate according to finalized previous round, new fork required
	if raw := TransactionMintWorkHacks[batch]; raw != "" && node.isMainnet() {
		rt, err := hex.DecodeString(raw)
		if err != nil {
			panic(raw)
		}
		ver, err := common.UnmarshalVersionedTransaction(rt)
		if err != nil {
			panic(raw)
		}
		return ver
	}

	if node.isMainnet() && batch < MainnetMintTransactionV2ForkBatch {
		return node.buildMintTransactionV1(timestamp, validateOnly)
	}

	accepted := node.NodesListWithoutState(timestamp, true)
	mints, err := node.distributeKernelMintByWorks(accepted, amount, timestamp)
	if err != nil {
		logger.Printf("buildLegacyKerneNodeMintTransaction ERROR %s\n", err.Error())
		return nil
	}

	tx := node.NewTransaction(common.XINAssetId)
	tx.AddKernelNodeMintInputLegacy(uint64(batch), amount)
	script := common.NewThresholdScript(1)
	total := common.NewInteger(0)
	for _, m := range mints {
		in := fmt.Sprintf("MINTKERNELNODE%d", batch)
		si := crypto.NewHash([]byte(m.Signer.String() + in))
		seed := append(si[:], si[:]...)
		tx.AddScriptOutput([]*common.Address{&m.Payee}, script, m.Work, seed)
		total = total.Add(m.Work)
	}
	if total.Cmp(amount) > 0 {
		panic(fmt.Errorf("buildLegacyKerneNodeMintTransaction %s %s", amount, total))
	}

	if diff := amount.Sub(total); diff.Sign() > 0 {
		addr := common.NewAddressFromSeed(make([]byte, 64))
		script := common.NewThresholdScript(common.Operator64)
		in := fmt.Sprintf("MINTKERNELNODE%dDIFF", batch)
		si := crypto.NewHash([]byte(addr.String() + in))
		seed := append(si[:], si[:]...)
		tx.AddScriptOutput([]*common.Address{&addr}, script, diff, seed)
	}
	return tx.AsVersioned()
}

func (node *Node) tryToMintKernelNodeLegacy() error {
	signed := node.buildLegacyKerneNodeMintTransaction(node.GraphTimestamp, false)
	if signed == nil {
		return nil
	}

	if signed.Version == 1 {
		err := signed.SignInputV1(node.persistStore, 0, []*common.Address{&node.Signer})
		if err != nil {
			return err
		}
	} else {
		err := signed.SignInput(node.persistStore, 0, []*common.Address{&node.Signer})
		if err != nil {
			return err
		}
	}
	err := signed.Validate(node.persistStore, false)
	if err != nil {
		return err
	}
	err = node.persistStore.CachePutTransaction(signed)
	if err != nil {
		return err
	}
	s := &common.Snapshot{
		Version: common.SnapshotVersionCommonEncoding,
		NodeId:  node.IdForNetwork,
	}
	s.AddSoleTransaction(signed.PayloadHash())
	logger.Println("tryToMintKernelNodeLegacy", signed.PayloadHash(), hex.EncodeToString(signed.Marshal()))
	return node.chain.AppendSelfEmpty(s)
}

func (node *Node) validateMintSnapshot(snap *common.Snapshot, tx *common.VersionedTransaction) error {
	timestamp := snap.Timestamp
	if snap.Timestamp == 0 && snap.NodeId == node.IdForNetwork {
		timestamp = uint64(clock.Now().UnixNano())
	}

	var signed *common.VersionedTransaction
	cur, err := node.persistStore.ReadCustodian(timestamp)
	if err != nil {
		return err
	}
	if cur == nil && node.isMainnet() {
		signed = node.buildLegacyKerneNodeMintTransaction(timestamp, true)
		if signed == nil {
			return fmt.Errorf("no legacy mint available at %d", timestamp)
		}
	} else {
		signed = node.buildUniversalMintTransaction(cur, timestamp, true)
		if signed == nil {
			return fmt.Errorf("no universal mint available at %d", timestamp)
		}
	}

	if tx.PayloadHash() != signed.PayloadHash() {
		th := hex.EncodeToString(tx.PayloadMarshal())
		sh := hex.EncodeToString(signed.PayloadMarshal())
		return fmt.Errorf("malformed mint transaction at %d %s %s", timestamp, th, sh)
	}
	return nil
}

func (node *Node) checkUniversalMintPossibility(timestamp uint64, validateOnly bool) (int, common.Integer) {
	if timestamp <= node.Epoch {
		return 0, common.Zero
	}

	since := timestamp - node.Epoch
	hours := int(since / 3600000000000)
	batch := hours / 24
	if batch < 1 {
		return 0, common.Zero
	}
	kmb, kme := config.KernelMintTimeBegin, config.KernelMintTimeEnd
	if hours%24 < kmb || hours%24 > kme {
		return 0, common.Zero
	}

	pool := MintPool
	for i := 0; i < batch/MintYearBatches; i++ {
		pool = pool.Sub(pool.Div(MintYearShares))
	}
	pool = pool.Div(MintYearShares)
	total := pool.Div(MintYearBatches)

	dist, err := node.persistStore.ReadLastMintDistribution(^uint64(0))
	if err != nil {
		logger.Verbosef("ReadLastMintDistribution ERROR %s\n", err)
		return 0, common.Zero
	}
	logger.Verbosef("checkUniversalMintPossibility OLD %s %s %d %s %d\n",
		pool, total, batch, dist.Amount, dist.Batch)

	if batch < int(dist.Batch) {
		return 0, common.Zero
	}
	if batch == int(dist.Batch) {
		if validateOnly {
			return batch, dist.Amount
		}
		return 0, common.Zero
	}

	amount := total.Mul(batch - int(dist.Batch))
	logger.Verbosef("checkUniversalMintPossibility NEW %s %s %s %d %s %d\n",
		pool, total, amount, batch, dist.Amount, dist.Batch)
	return batch, amount
}

func (node *Node) checkLegacyMintPossibility(timestamp uint64, validateOnly bool) (int, common.Integer) {
	if timestamp <= node.Epoch {
		return 0, common.Zero
	}

	since := timestamp - node.Epoch
	hours := int(since / 3600000000000)
	batch := hours / 24
	if batch < 1 {
		return 0, common.Zero
	}
	kmb, kme := config.KernelMintTimeBegin, config.KernelMintTimeEnd
	if node.isMainnet() && batch < MainnetMintPeriodForkBatch {
		kmb = MainnetMintPeriodForkTimeBegin
		kme = MainnetMintPeriodForkTimeEnd
	}
	if hours%24 < kmb || hours%24 > kme {
		return 0, common.Zero
	}

	pool := MintPool
	for i := 0; i < batch/MintYearBatches; i++ {
		pool = pool.Sub(pool.Div(MintYearShares))
	}
	pool = pool.Div(MintYearShares)
	total := pool.Div(MintYearBatches)
	light := total.Div(10)
	full := light.Mul(9)

	dist, err := node.persistStore.ReadLastMintDistribution(^uint64(0))
	if err != nil {
		logger.Verbosef("ReadLastMintDistribution ERROR %s\n", err)
		return 0, common.Zero
	}
	logger.Verbosef("checkMintPossibility OLD %s %s %s %s %d %s %d\n",
		pool, total, light, full, batch, dist.Amount, dist.Batch)

	if batch < int(dist.Batch) {
		return 0, common.Zero
	}
	if batch == int(dist.Batch) {
		if validateOnly {
			return batch, dist.Amount
		}
		return 0, common.Zero
	}

	amount := full.Mul(batch - int(dist.Batch))
	logger.Verbosef("checkMintPossibility NEW %s %s %s %s %s %d %s %d\n",
		pool, total, light, full, amount, batch, dist.Amount, dist.Batch)
	return batch, amount
}

type CNodeWork struct {
	CNode
	Work common.Integer
}

func (node *Node) ListMintWorks(batch uint64) (map[crypto.Hash][2]uint64, error) {
	now := node.Epoch + batch*uint64(time.Hour*24)
	list := node.NodesListWithoutState(now, true)
	cids := make([]crypto.Hash, len(list))
	for i, n := range list {
		cids[i] = n.IdForNetwork
	}
	day := now / (uint64(time.Hour) * 24)
	works, err := node.persistStore.ListNodeWorks(cids, uint32(day))
	return works, err
}

func (node *Node) ListRoundSpaces(cids []crypto.Hash, day uint64) (map[crypto.Hash][]*common.RoundSpace, error) {
	epoch := node.Epoch / (uint64(time.Hour) * 24)
	spaces := make(map[crypto.Hash][]*common.RoundSpace)
	for _, id := range cids {
		ns, err := node.persistStore.ReadNodeRoundSpacesForBatch(id, day-epoch)
		if err != nil {
			return nil, err
		}
		spaces[id] = ns
	}
	return spaces, nil
}

// a = average work
// for x > 7a, y = 2a
// for 7a > x > a, y = 1/6x + 5/6a
// for a > x > 1/7a, y = x
// for x < 1/7a, y = 1/7a
func (node *Node) distributeKernelMintByWorks(accepted []*CNode, base common.Integer, timestamp uint64) ([]*CNodeWork, error) {
	mints := make([]*CNodeWork, len(accepted))
	cids := make([]crypto.Hash, len(accepted))
	for i, n := range accepted {
		cids[i] = n.IdForNetwork
		mints[i] = &CNodeWork{CNode: *n}
	}
	epoch := node.Epoch / (uint64(time.Hour) * 24)
	day := timestamp / (uint64(time.Hour) * 24)
	if day < epoch {
		panic(fmt.Errorf("invalid mint day %d %d", epoch, day))
	}
	if day-epoch == 0 {
		work := base.Div(len(mints))
		for _, m := range mints {
			m.Work = work
		}
		return mints, nil
	}

	thr := int(node.ConsensusThreshold(timestamp, false))
	err := node.validateWorksAndSpacesAggregator(cids, thr, day)
	if err != nil {
		return nil, fmt.Errorf("distributeKernelMintByWorks not ready yet %d %v", day, err)
	}

	works, err := node.persistStore.ListNodeWorks(cids, uint32(day)-1)
	if err != nil {
		return nil, err
	}
	spaces, err := node.ListRoundSpaces(cids, day-1)
	if err != nil {
		return nil, err
	}

	var valid int
	var minW, maxW, totalW common.Integer
	for _, m := range mints {
		ns := spaces[m.IdForNetwork]
		if len(ns) > 0 {
			// TODO use this for universal mint distributions
			logger.Printf("node spaces %s %d %d\n", m.IdForNetwork, ns[0].Batch, len(ns))
		}

		w := works[m.IdForNetwork]
		m.Work = common.NewInteger(w[0]).Mul(120).Div(100)
		sign := common.NewInteger(w[1])
		if sign.Sign() > 0 {
			m.Work = m.Work.Add(sign)
		}
		if m.Work.Sign() == 0 {
			continue
		}
		valid += 1
		if minW.Sign() == 0 {
			minW = m.Work
		} else if m.Work.Cmp(minW) < 0 {
			minW = m.Work
		}
		if m.Work.Cmp(maxW) > 0 {
			maxW = m.Work
		}
		totalW = totalW.Add(m.Work)
	}
	if valid < thr {
		return nil, fmt.Errorf("distributeKernelMintByWorks not valid %d %d %d %d",
			day, len(mints), thr, valid)
	}

	totalW = totalW.Sub(minW).Sub(maxW)
	avg := totalW.Div(valid - 2)
	if avg.Sign() == 0 {
		return nil, fmt.Errorf("distributeKernelMintByWorks not valid %d %d %d %d",
			day, len(mints), thr, valid)
	}

	totalW = common.NewInteger(0)
	upper, lower := avg.Mul(7), avg.Div(7)
	for _, m := range mints {
		if m.Work.Cmp(upper) >= 0 {
			m.Work = avg.Mul(2)
		} else if m.Work.Cmp(avg) >= 0 {
			m.Work = m.Work.Div(6).Add(avg.Mul(5).Div(6))
		} else if m.Work.Cmp(lower) <= 0 {
			m.Work = avg.Div(7)
		}
		totalW = totalW.Add(m.Work)
	}

	for _, m := range mints {
		rat := m.Work.Ration(totalW)
		m.Work = rat.Product(base)
	}
	return mints, nil
}

func (node *Node) validateWorksAndSpacesAggregator(cids []crypto.Hash, thr int, day uint64) error {
	worksAgg, spacesAgg := 0, 0

	works, err := node.persistStore.ListNodeWorks(cids, uint32(day))
	if err != nil {
		return err
	}
	for _, w := range works {
		if w[0] > 0 {
			worksAgg += 1
		}
	}
	if worksAgg < thr {
		return fmt.Errorf("validateWorksAndSpacesAggregator works not ready yet %d %d %d %d",
			day, len(works), worksAgg, thr)
	}

	spaces, err := node.persistStore.ListAggregatedRoundSpaceCheckpoints(cids)
	if err != nil {
		return err
	}
	epoch := node.Epoch / (uint64(time.Hour) * 24)
	batch := day - epoch
	for _, s := range spaces {
		if s.Batch >= batch {
			spacesAgg += 1
		}
	}
	if spacesAgg < thr || worksAgg != spacesAgg {
		return fmt.Errorf("validateWorksAndSpacesAggregator spaces not ready yet %d %d %d %d %d",
			batch, len(spaces), spacesAgg, worksAgg, thr)
	}

	return nil
}
