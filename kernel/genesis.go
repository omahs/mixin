package kernel

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/crypto"
)

const (
	MinimumNodeCount = 7
	PledgeAmount     = 10000
)

type Genesis struct {
	Epoch int64 `json:"epoch"`
	Nodes []struct {
		Address common.Address `json:"address"`
		Balance common.Integer `json:"balance"`
	} `json:"nodes"`
}

func (node *Node) LoadGenesis(configDir string) error {
	const stateKeyNetwork = "network"

	gns, err := readGenesis(configDir + "/genesis.json")
	if err != nil {
		return err
	}
	for _, in := range gns.Nodes {
		node.ConsensusNodes = append(node.ConsensusNodes, in.Address)
	}

	data, err := json.Marshal(gns)
	if err != nil {
		return err
	}
	node.networkId = crypto.NewHash(data)
	node.IdForNetwork = node.Account.Hash().ForNetwork(node.networkId)

	var state struct {
		Id crypto.Hash
	}
	found, err := node.store.StateGet(stateKeyNetwork, &state)
	if err != nil || state.Id == node.networkId {
		return err
	}
	if found {
		return fmt.Errorf("invalid genesis for network %s", state.Id.String())
	}

	var snapshots []*common.SnapshotWithTopologicalOrder
	for i, in := range gns.Nodes {
		seed := crypto.NewHash([]byte(in.Address.String() + "NODEPLEDGE"))
		r := crypto.NewKeyFromSeed(append(seed[:], seed[:]...))
		R := r.Public()
		var keys []crypto.Key
		for _, d := range gns.Nodes {
			key := crypto.DeriveGhostPublicKey(&r, &d.Address.PublicViewKey, &d.Address.PublicSpendKey)
			keys = append(keys, *key)
		}

		tx := common.Transaction{
			Version: common.TxVersion,
			Asset:   common.XINAssetId,
			Inputs: []*common.Input{
				{
					Hash:  crypto.Hash{},
					Index: i,
				},
			},
			Outputs: []*common.Output{
				{
					Type:   common.OutputTypeNodePledge,
					Script: common.Script([]uint8{common.OperatorCmp, common.OperatorSum, uint8(len(gns.Nodes)*2/3 + 1)}),
					Amount: common.NewInteger(PledgeAmount),
					Keys:   keys,
					Mask:   R,
				},
			},
			Extra: in.Address.PublicSpendKey[:],
		}

		remaining := in.Balance.Sub(common.NewInteger(PledgeAmount))
		if remaining.Cmp(common.NewInteger(0)) > 0 {
			seed := crypto.NewHash([]byte(in.Address.String() + "NODEREMAINING"))
			r := crypto.NewKeyFromSeed(append(seed[:], seed[:]...))
			R := r.Public()
			key := crypto.DeriveGhostPublicKey(&r, &in.Address.PublicViewKey, &in.Address.PublicSpendKey)
			tx.Outputs = append(tx.Outputs, &common.Output{
				Type:   common.OutputTypeScript,
				Script: common.Script([]uint8{common.OperatorCmp, common.OperatorSum, 1}),
				Amount: remaining,
				Keys:   []crypto.Key{*key},
				Mask:   R,
			})
		}

		signed := &common.SignedTransaction{Transaction: tx}
		nodeId := in.Address.Hash().ForNetwork(node.networkId)
		snapshot := common.Snapshot{
			NodeId:      nodeId,
			Transaction: signed,
			RoundNumber: 0,
			Timestamp:   uint64(time.Unix(gns.Epoch, 0).UnixNano()),
		}
		topo := &common.SnapshotWithTopologicalOrder{
			Snapshot:         snapshot,
			TopologicalOrder: node.TopoCounter.Next(),
		}
		snapshots = append(snapshots, topo)
	}
	err = node.store.SnapshotsLoadGenesis(snapshots)
	if err != nil {
		return err
	}

	state.Id = node.networkId
	return node.store.StateSet(stateKeyNetwork, state)
}

func readGenesis(path string) (*Genesis, error) {
	f, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var gns Genesis
	err = json.Unmarshal(f, &gns)
	if err != nil {
		return nil, err
	}
	if len(gns.Nodes) != MinimumNodeCount {
		return nil, fmt.Errorf("invalid genesis inputs number %d/%d", len(gns.Nodes), MinimumNodeCount)
	}

	inputsFilter := make(map[string]bool)
	for _, in := range gns.Nodes {
		_, err := common.NewAddressFromString(in.Address.String())
		if err != nil {
			return nil, err
		}
		if in.Balance.Cmp(common.NewInteger(PledgeAmount)) < 0 {
			return nil, fmt.Errorf("invalid genesis input amount %s", in.Balance.String())
		}
		if inputsFilter[in.Address.String()] {
			return nil, fmt.Errorf("duplicated genesis inputs %s", in.Address.String())
		}
		seed := crypto.NewHash(in.Address.PublicSpendKey[:])
		privateView := crypto.NewKeyFromSeed(append(seed[:], seed[:]...))
		if privateView.Public() != in.Address.PublicViewKey {
			return nil, fmt.Errorf("invalid node key format %s %s", privateView.Public().String(), in.Address.PublicViewKey.String())
		}
	}
	return &gns, nil
}
