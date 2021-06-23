// Copyright 2017 The go-ethereum Authors
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

// Package clique implements the proof-of-authority consensus engine.
package aura

import (
	"sort"

	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/math"
	"github.com/ledgerwatch/erigon/common/u256"
	"github.com/ledgerwatch/erigon/consensus/aura/auraabi"
)

type StepDuration struct {
	Single      *uint64           // Duration of all steps.
	Transitions map[uint64]uint64 // Step duration transitions: a mapping of timestamp to step durations.
}

// Draws an validator nonce modulo number of validators.
func GetFromValidatorSet(set ValidatorSet, parent common.Hash, nonce uint) (common.Address, error) {
	d, err := set.defaultCaller(parent)
	if err != nil {
		return common.Address{}, err
	}
	return set.getWithCaller(parent, nonce, d)
}

type BlockReward struct {
	Single *uint64
	Multi  map[uint64]uint64
}

// Different ways of specifying validators.
type ValidatorSetJson struct {
	// A simple list of authorities.
	List []common.Address `json:"list"`
	// Address of a contract that indicates the list of authorities.
	SafeContract *common.Address `json:"safeContract"`
	// Address of a contract that indicates the list of authorities and enables reporting of theor misbehaviour using transactions.
	Contract *common.Address `json:"contract"`
	// A map of starting blocks for each validator set.
	Multi map[uint64]*ValidatorSetJson `json:"multi"`
}

func newValidatorSetFromJson(j *ValidatorSetJson, posdaoTransition *uint64) ValidatorSet {
	if j.List != nil {
		return &SimpleList{validators: j.List}
	}
	if j.SafeContract != nil {
		return &ValidatorSafeContract{contractAddress: *j.SafeContract}
	}
	if j.Contract != nil {
		return &ValidatorContract{contractAddress: *j.SafeContract, posdaoTransition: posdaoTransition}
	}
	if j.Multi != nil {
		l := map[uint64]ValidatorSet{}
		for block, set := range j.Multi {
			l[block] = newValidatorSetFromJson(set, posdaoTransition)
		}
		return NewMulti(l)
	}

	return nil
}

type JsonSpec struct {
	StepDuration StepDuration      // Block duration, in seconds.
	Validators   *ValidatorSetJson // Valid authorities

	// Starting step. Determined automatically if not specified.
	// To be used for testing only.
	StartStep               *uint64
	ValidateScoreTransition *uint64      // Block at which score validation should start.
	ValidateStepTransition  *uint64      // Block from which monotonic steps start.
	ImmediateTransitions    *bool        // Whether transitions should be immediate.
	BlockReward             *BlockReward // Reward per block in wei.
	// Block at which the block reward contract should start being used. This option allows one to
	// add a single block reward contract transition and is compatible with the multiple address
	// option `block_reward_contract_transitions` below.
	BlockRewardContractTransition *uint64
	/// Block reward contract address which overrides the `block_reward` setting. This option allows
	/// one to add a single block reward contract address and is compatible with the multiple
	/// address option `block_reward_contract_transitions` below.
	BlockRewardContractAddress *common.Address
	// Block reward contract addresses with their associated starting block numbers.
	//
	// Setting the block reward contract overrides `block_reward`. If the single block reward
	// contract address is also present then it is added into the map at the block number stored in
	// `block_reward_contract_transition` or 0 if that block number is not provided. Therefore both
	// a single block reward contract transition and a map of reward contract transitions can be
	// used simulataneously in the same configuration. In such a case the code requires that the
	// block number of the single transition is strictly less than any of the block numbers in the
	// map.
	BlockRewardContractTransitions map[uint]common.Address
	/// Block reward code. This overrides the block reward contract address.
	BlockRewardContractCode []byte
	// Block at which maximum uncle count should be considered.
	MaximumUncleCountTransition *uint64
	// Maximum number of accepted uncles.
	MaximumUncleCount *uint
	// Block at which empty step messages should start.
	EmptyStepsTransition *uint64
	// Maximum number of accepted empty steps.
	MaximumEmptySteps *uint
	// Strict validation of empty steps transition block.
	StrictEmptyStepsTransition *uint
	// First block for which a 2/3 quorum (instead of 1/2) is required.
	TwoThirdsMajorityTransition *uint64
	// The random number contract's address, or a map of contract transitions.
	RandomnessContractAddress map[uint64]common.Address
	// The addresses of contracts that determine the block gas limit starting from the block number
	// associated with each of those contracts.
	BlockGasLimitContractTransitions map[uint64]common.Address
	// The block number at which the consensus engine switches from AuRa to AuRa with POSDAO
	// modifications.
	PosdaoTransition *uint64
}

type Code struct {
	Code     []byte
	CodeHash common.Hash
}

type BlockRewardContract struct {
	BlockNum uint64
	Address  common.Address // On-chain address.
}

func NewBlockRewardContract(address common.Address) *BlockRewardContract {
	return &BlockRewardContract{Address: address}
}

type BlockRewardContractList []*BlockRewardContract

func (r BlockRewardContractList) Less(i, j int) bool { return r[i].BlockNum < r[j].BlockNum }
func (r BlockRewardContractList) Len() int           { return len(r) }
func (r BlockRewardContractList) Swap(i, j int)      { r[i], r[j] = r[j], r[i] }
func (r BlockRewardContractList) GreaterOrEqual(block uint64) *BlockRewardContract {
	return r[sort.Search(len(r), func(i int) bool { return block >= r[i].BlockNum })]
}

type AuthorityRoundParams struct {
	// A map defining intervals of blocks with the given times (in seconds) to wait before next
	// block or authority switching. The keys in the map are steps of starting blocks of those
	// periods. The entry at `0` should be defined.
	//
	// Wait times (durations) are additionally required to be less than 65535 since larger values
	// lead to slow block issuance.
	StepDurations map[uint64]uint64
	// Starting step,
	StartStep *uint64
	// Valid validators.
	Validators ValidatorSet
	// Chain score validation transition block.
	ValidateScoreTransition uint64
	// Monotonic step validation transition block.
	ValidateStepTransition uint64
	// Immediate transitions.
	ImmediateTransitions bool
	// Block reward in base units.
	BlockReward map[uint64]*uint256.Int
	// Block reward contract addresses with their associated starting block numbers.
	BlockRewardContractTransitions map[uint64]*auraabi.BlockRewardCaller
	// Number of accepted uncles transition block.
	MaximumUncleCountTransition uint64
	// Number of accepted uncles.
	MaximumUncleCount uint
	// Empty step messages transition block.
	EmptyStepsTransition uint64
	// First block for which a 2/3 quorum (instead of 1/2) is required.
	TwoThirdsMajorityTransition uint64
	// Number of accepted empty steps.
	MaximumEmptySteps uint
	// Transition block to strict empty steps validation.
	StrictEmptyStepsTransition uint64
	// If set, enables random number contract integration. It maps the transition block to the contract address.
	RandomnessContractAddress map[uint64]common.Address
	// The addresses of contracts that determine the block gas limit with their associated block
	// numbers.
	BlockGasLimitContractTransitions map[uint64]common.Address
	// If set, this is the block number at which the consensus engine switches from AuRa to AuRa
	// with POSDAO modifications.
	PosdaoTransition *uint64
}

func FromJson(jsonParams JsonSpec) (AuthorityRoundParams, error) {
	params := AuthorityRoundParams{
		Validators:                       newValidatorSetFromJson(jsonParams.Validators, jsonParams.PosdaoTransition),
		StartStep:                        jsonParams.StartStep,
		RandomnessContractAddress:        jsonParams.RandomnessContractAddress,
		BlockGasLimitContractTransitions: jsonParams.BlockGasLimitContractTransitions,
		PosdaoTransition:                 jsonParams.PosdaoTransition,
	}
	if jsonParams.StepDuration.Single != nil {
		params.StepDurations[0] = *jsonParams.StepDuration.Single
	} else if jsonParams.StepDuration.Transitions != nil {
		params.StepDurations = jsonParams.StepDuration.Transitions
	}

	//TODO: jsonParams.BlockRewardContractTransitions
	/*
			   let mut br_transitions: BTreeMap<_, _> = p
		           .block_reward_contract_transitions
		           .unwrap_or_default()
		           .into_iter()
		           .map(|(block_num, address)| {
		               (
		                   block_num.into(),
		                   BlockRewardContract::new_from_address(address.into()),
		               )
		           })
		           .collect();
	*/

	transitionBlockNum := uint64(0)
	if jsonParams.BlockRewardContractTransition != nil {
		transitionBlockNum = *jsonParams.BlockRewardContractTransition
	}
	/*
	   if (p.block_reward_contract_code.is_some() || p.block_reward_contract_address.is_some())
	        && br_transitions
	            .keys()
	            .next()
	            .map_or(false, |&block_num| block_num <= transition_block_num)
	    {
	        let s = "blockRewardContractTransition";
	        panic!("{} should be less than any of the keys in {}s", s, s);
	    }
	*/
	if jsonParams.BlockRewardContractCode != nil {
		/* TODO: support hard-coded reward contract
		    br_transitions.insert(
		       transition_block_num,
		       BlockRewardContract::new_from_code(Arc::new(code.into())),
		   );
		*/
	} else if jsonParams.BlockRewardContractAddress != nil {
		var err error
		params.BlockRewardContractTransitions[transitionBlockNum], err = auraabi.NewBlockRewardCaller(*jsonParams.BlockRewardContractAddress, nil)
		if err != nil {
			return params, err
		}
	}

	if jsonParams.ValidateScoreTransition != nil {
		params.ValidateScoreTransition = *jsonParams.ValidateScoreTransition
	}
	if jsonParams.ValidateStepTransition != nil {
		params.ValidateStepTransition = *jsonParams.ValidateStepTransition
	}
	if jsonParams.ImmediateTransitions != nil {
		params.ImmediateTransitions = *jsonParams.ImmediateTransitions
	}
	if jsonParams.MaximumUncleCount != nil {
		params.MaximumUncleCount = *jsonParams.MaximumUncleCount
	}
	if jsonParams.MaximumUncleCountTransition != nil {
		params.MaximumUncleCountTransition = *jsonParams.MaximumUncleCountTransition
	}
	if jsonParams.MaximumEmptySteps != nil {
		params.MaximumEmptySteps = *jsonParams.MaximumEmptySteps
	}
	if jsonParams.EmptyStepsTransition != nil {
		params.EmptyStepsTransition = *jsonParams.EmptyStepsTransition
	}

	params.BlockReward = map[uint64]*uint256.Int{}
	if jsonParams.BlockReward == nil {
		params.BlockReward[0] = u256.Num0
	} else {
		if jsonParams.BlockReward.Single != nil {
			params.BlockReward[0] = uint256.NewInt(*jsonParams.BlockReward.Single)
		} else if jsonParams.BlockReward.Multi != nil {
			// add block reward from genesis and put reward to zero.
			params.BlockReward[0] = u256.Num0
			for block, reward := range jsonParams.BlockReward.Multi {
				params.BlockReward[block] = uint256.NewInt(reward)
			}
		}
	}

	params.EmptyStepsTransition = math.MaxUint64
	if jsonParams.EmptyStepsTransition != nil {
		if *jsonParams.EmptyStepsTransition < 1 {
			params.EmptyStepsTransition = 1
		} else {
			params.EmptyStepsTransition = *jsonParams.EmptyStepsTransition
		}
	}

	params.TwoThirdsMajorityTransition = math.MaxUint64
	if jsonParams.TwoThirdsMajorityTransition != nil {
		params.TwoThirdsMajorityTransition = *jsonParams.TwoThirdsMajorityTransition
	}
	return params, nil
}
