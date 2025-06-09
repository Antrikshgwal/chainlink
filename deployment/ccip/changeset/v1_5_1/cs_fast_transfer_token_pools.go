package v1_5_1

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/Masterminds/semver/v3"
	"github.com/ethereum/go-ethereum/common"
	"github.com/smartcontractkit/chainlink-ccip/chains/evm/gobindings/generated/latest/fast_transfer_token_pool_abstract"

	cldf "github.com/smartcontractkit/chainlink-deployments-framework/deployment"
	"github.com/smartcontractkit/chainlink/deployment/ccip/shared"
	"github.com/smartcontractkit/chainlink/deployment/ccip/shared/deployergroup"
	"github.com/smartcontractkit/chainlink/deployment/ccip/shared/stateview"
	"github.com/smartcontractkit/chainlink/deployment/ccip/shared/stateview/evm"
	"github.com/smartcontractkit/chainlink/deployment/common/proposalutils"
)

var _ cldf.ChangeSet[FastTransferUpdateLaneConfigConfig] = FastTransferUpdateLaneConfigChangeset

var (
	MAX_FAST_TRANSFER_FILLER_FEE_BPS = uint16(10000)
)

type UpdateLaneConfig struct {
	FastTransferFillerFeeBps uint16
	FastTransferPoolFeeBps   uint16
	FillAmountMaxRequest     *big.Int
	FillerAllowlistEnabled   bool
	SkipAllowlistValidation  bool
}

func (u UpdateLaneConfig) Validate(contract *fast_transfer_token_pool_abstract.FastTransferTokenPoolAbstract) error {
	if u.FastTransferFillerFeeBps > MAX_FAST_TRANSFER_FILLER_FEE_BPS {
		return fmt.Errorf("fast transfer filler fee bps %d is greater than %d", u.FastTransferFillerFeeBps, MAX_FAST_TRANSFER_FILLER_FEE_BPS)
	}
	if u.FastTransferPoolFeeBps > MAX_FAST_TRANSFER_FILLER_FEE_BPS {
		return fmt.Errorf("fast transfer pool fee bps %d is greater than %d", u.FastTransferPoolFeeBps, MAX_FAST_TRANSFER_FILLER_FEE_BPS)
	}
	if u.FillAmountMaxRequest == nil || u.FillAmountMaxRequest.Sign() <= 0 {
		return errors.New("fill amount max request must be a positive integer")
	}

	allowedFiller, err := contract.GetAllowedFillers(nil)
	if err != nil {
		return fmt.Errorf("failed to get allowed fillers: %w", err)
	}

	if !u.SkipAllowlistValidation && u.FillerAllowlistEnabled && len(allowedFiller) == 0 {
		return errors.New("filler allowlist is enabled but no fillers are allowed")
	}

	return nil
}

type FillerAllowlistConfig struct {
	AddFillers    []common.Address
	RemoveFillers []common.Address
}

func (f FillerAllowlistConfig) Validate(contract *fast_transfer_token_pool_abstract.FastTransferTokenPoolAbstract) error {
	if len(f.AddFillers) == 0 && len(f.RemoveFillers) == 0 {
		return errors.New("at least one filler must be added or removed")
	}
	for _, filler := range f.AddFillers {
		if filler == (common.Address{}) {
			return errors.New("filler address cannot be empty")
		}
	}
	for _, filler := range f.RemoveFillers {
		if filler == (common.Address{}) {
			return errors.New("filler address cannot be empty")
		}
	}

	allowedFillers, err := contract.GetAllowedFillers(nil)
	if err != nil {
		return fmt.Errorf("failed to get allowed fillers: %w", err)
	}
	for _, filler := range f.AddFillers {
		for _, allowedFiller := range allowedFillers {
			if filler == allowedFiller {
				return fmt.Errorf("filler %s is already in the allowlist", filler.Hex())
			}
		}
	}
	for _, filler := range f.RemoveFillers {
		found := false
		for _, allowedFiller := range allowedFillers {
			if filler == allowedFiller {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("filler %s is not in the allowlist", filler.Hex())
		}
	}

	return nil
}

type FastTransferUpdateLaneConfigConfig struct {
	TokenSymbol     shared.TokenSymbol
	ContractType    cldf.ContractType
	ContractVersion semver.Version
	Updates         map[uint64](map[uint64]UpdateLaneConfig)
	// MCMS defines the delay to use for Timelock (if absent, the changeset will attempt to use the deployer key).
	MCMS *proposalutils.TimelockConfig
}

func (c FastTransferUpdateLaneConfigConfig) Validate(env cldf.Environment) error {
	if c.TokenSymbol == "" {
		return errors.New("token symbol must be defined")
	}
	state, err := stateview.LoadOnchainState(env)
	if err != nil {
		return fmt.Errorf("failed to load onchain state: %w", err)
	}
	for chainSelector, poolUpdate := range c.Updates {
		err := cldf.IsValidChainSelector(chainSelector)
		if err != nil {
			return fmt.Errorf("failed to validate chain selector %d: %w", chainSelector, err)
		}
		chain, ok := env.BlockChains.EVMChains()[chainSelector]
		if !ok {
			return fmt.Errorf("chain with selector %d does not exist in environment", chainSelector)
		}
		chainState, ok := state.Chains[chainSelector]
		if !ok {
			return fmt.Errorf("%s does not exist in state", chain.String())
		}

		if err := validateFastTransferTokenPoolExists(chainState, c.TokenSymbol, c.ContractType, c.ContractVersion, chain.String()); err != nil {
			return err
		}

		if c.MCMS != nil {
			if timelock := chainState.Timelock; timelock == nil {
				return fmt.Errorf("missing timelock on %s", chain.String())
			}
			if proposerMcm := chainState.ProposerMcm; proposerMcm == nil {
				return fmt.Errorf("missing proposerMcm on %s", chain.String())
			}
		}

		pool, err := GetFastTransferTokenPoolContract(env, c.TokenSymbol, c.ContractType, c.ContractVersion, chainSelector)
		if err != nil {
			return fmt.Errorf("failed to get fast transfer token pool contract for %s token on chain %d: %w", c.TokenSymbol, chainSelector, err)
		}

		for _, update := range poolUpdate {
			err := update.Validate(pool)
			if err != nil {
				return fmt.Errorf("failed to validate update for chain selector %d: %w", chainSelector, err)
			}
		}
	}
	return nil
}

type FastTransferFillerAllowlistConfig struct {
	TokenSymbol     shared.TokenSymbol
	ContractType    cldf.ContractType
	ContractVersion semver.Version
	Updates         map[uint64]FillerAllowlistConfig
	// MCMS defines the delay to use for Timelock (if absent, the changeset will attempt to use the deployer key).
	MCMS *proposalutils.TimelockConfig
}

func (c FastTransferFillerAllowlistConfig) Validate(env cldf.Environment) error {
	if c.TokenSymbol == "" {
		return errors.New("token symbol must be defined")
	}
	state, err := stateview.LoadOnchainState(env)
	if err != nil {
		return fmt.Errorf("failed to load onchain state: %w", err)
	}
	for chainSelector, update := range c.Updates {
		err := cldf.IsValidChainSelector(chainSelector)
		if err != nil {
			return fmt.Errorf("failed to validate chain selector %d: %w", chainSelector, err)
		}
		chain, ok := env.BlockChains.EVMChains()[chainSelector]
		if !ok {
			return fmt.Errorf("chain with selector %d does not exist in environment", chainSelector)
		}
		chainState, ok := state.Chains[chainSelector]
		if !ok {
			return fmt.Errorf("%s does not exist in state", chain.String())
		}

		if err := validateFastTransferTokenPoolExists(chainState, c.TokenSymbol, c.ContractType, c.ContractVersion, chain.String()); err != nil {
			return err
		}

		if c.MCMS != nil {
			if timelock := chainState.Timelock; timelock == nil {
				return fmt.Errorf("missing timelock on %s", chain.String())
			}
			if proposerMcm := chainState.ProposerMcm; proposerMcm == nil {
				return fmt.Errorf("missing proposerMcm on %s", chain.String())
			}
		}

		pool, err := GetFastTransferTokenPoolContract(env, c.TokenSymbol, c.ContractType, c.ContractVersion, chainSelector)
		if err != nil {
			return fmt.Errorf("failed to get fast transfer token pool contract for %s token on chain %d: %w", c.TokenSymbol, chainSelector, err)
		}

		err = update.Validate(pool)
		if err != nil {
			return fmt.Errorf("failed to validate filler allowlist update for chain selector %d: %w", chainSelector, err)
		}

	}
	return nil
}

func validateFastTransferTokenPoolExists(chainState evm.CCIPChainState, tokenSymbol shared.TokenSymbol, contractType cldf.ContractType, contractVersion semver.Version, chainString string) error {
	switch contractType {
	case shared.BurnMintFastTransferTokenPool:
		if _, ok := chainState.BurnMintFastTransferTokenPools[tokenSymbol]; !ok {
			return fmt.Errorf("token %s does not have a fast transfer token pool on %s", tokenSymbol, chainString)
		}
		if _, ok := chainState.BurnMintFastTransferTokenPools[tokenSymbol][contractVersion]; !ok {
			return fmt.Errorf("token %s does not have a fast transfer token pool with version %s on %s", tokenSymbol, contractVersion.String(), chainString)
		}
	case shared.BurnMintWithExternalMinterFastTransferTokenPool:
		if _, ok := chainState.BurnMintWithExternalMinterFastTransferTokenPools[tokenSymbol]; !ok {
			return fmt.Errorf("token %s does not have a fast transfer token pool on %s", tokenSymbol, chainString)
		}
		if _, ok := chainState.BurnMintWithExternalMinterFastTransferTokenPools[tokenSymbol][contractVersion]; !ok {
			return fmt.Errorf("token %s does not have a fast transfer token pool with version %s on %s", tokenSymbol, contractVersion.String(), chainString)
		}
	default:
		return fmt.Errorf("unsupported contract type %s for fast transfer token pools", contractType)
	}
	return nil
}

func GetFastTransferTokenPoolContract(env cldf.Environment, tokenSymbol shared.TokenSymbol, contractType cldf.ContractType, contractVersion semver.Version, chainSelector uint64) (*fast_transfer_token_pool_abstract.FastTransferTokenPoolAbstract, error) {
	state, err := stateview.LoadOnchainState(env)
	if err != nil {
		return nil, fmt.Errorf("failed to load onchain state: %w", err)
	}

	chain, ok := env.BlockChains.EVMChains()[chainSelector]
	if !ok {
		return nil, fmt.Errorf("chain with selector %d does not exist in environment", chainSelector)
	}

	chainState, ok := state.Chains[chainSelector]
	if !ok {
		return nil, fmt.Errorf("%s does not exist in state", chain.String())
	}

	if err := validateFastTransferTokenPoolExists(chainState, tokenSymbol, contractType, contractVersion, chain.String()); err != nil {
		return nil, err
	}

	switch contractType {
	case shared.BurnMintFastTransferTokenPool:
		pool := chainState.BurnMintFastTransferTokenPools[tokenSymbol][contractVersion]
		return fast_transfer_token_pool_abstract.NewFastTransferTokenPoolAbstract(pool.Address(), env.BlockChains.EVMChains()[chainSelector].Client)
	case shared.BurnMintWithExternalMinterFastTransferTokenPool:
		pool := chainState.BurnMintWithExternalMinterFastTransferTokenPools[tokenSymbol][contractVersion]
		return fast_transfer_token_pool_abstract.NewFastTransferTokenPoolAbstract(pool.Address(), env.BlockChains.EVMChains()[chainSelector].Client)
	default:
		return nil, fmt.Errorf("unsupported contract type %s for fast transfer token pools", contractType)
	}
}

func FastTransferUpdateLaneConfigChangeset(env cldf.Environment, c FastTransferUpdateLaneConfigConfig) (cldf.ChangesetOutput, error) {
	if err := c.Validate(env); err != nil {
		return cldf.ChangesetOutput{}, fmt.Errorf("invalid FastTransferUpdateLaneConfigConfig: %w", err)
	}

	state, err := stateview.LoadOnchainState(env)
	if err != nil {
		return cldf.ChangesetOutput{}, fmt.Errorf("failed to load onchain state: %w", err)
	}

	deployerGroup := deployergroup.NewDeployerGroup(env, state, c.MCMS).WithDeploymentContext(fmt.Sprintf("configure %s token pools", c.TokenSymbol))

	for sourceChainSelector, updates := range c.Updates {
		deployer, err := deployerGroup.GetDeployer(sourceChainSelector)
		if err != nil {
			return cldf.ChangesetOutput{}, fmt.Errorf("failed to get deployer for chain selector %d: %w", sourceChainSelector, err)
		}
		pool, err := GetFastTransferTokenPoolContract(env, c.TokenSymbol, c.ContractType, c.ContractVersion, sourceChainSelector)
		if err != nil {
			return cldf.ChangesetOutput{}, fmt.Errorf("failed to get fast transfer token pool contract for %s token on chain %d: %w", c.TokenSymbol, sourceChainSelector, err)
		}
		laneConfigs := make([]fast_transfer_token_pool_abstract.FastTransferTokenPoolAbstractDestChainConfigUpdateArgs, 0)
		for destinationChainSelector, update := range updates {
			destinationPool, err := GetFastTransferTokenPoolContract(env, c.TokenSymbol, c.ContractType, c.ContractVersion, destinationChainSelector)
			if err != nil {
				return cldf.ChangesetOutput{}, fmt.Errorf("failed to get fast transfer token pool contract for %s token on chain %d: %w", c.TokenSymbol, destinationChainSelector, err)
			}
			laneConfigs = append(laneConfigs, fast_transfer_token_pool_abstract.FastTransferTokenPoolAbstractDestChainConfigUpdateArgs{
				MaxFillAmountPerRequest:  update.FillAmountMaxRequest,
				FastTransferFillerFeeBps: update.FastTransferFillerFeeBps,
				FastTransferPoolFeeBps:   update.FastTransferPoolFeeBps,
				RemoteChainSelector:      destinationChainSelector,
				DestinationPool:          common.LeftPadBytes(destinationPool.Address().Bytes(), 32),
				FillerAllowlistEnabled:   update.FillerAllowlistEnabled,
				// Left as default value for now
				SettlementOverheadGas: 0,
				ChainFamilySelector:   [4]byte{0x28, 0x12, 0xd5, 0x2c}, // Only EVM chains supported
				CustomExtraArgs:       []byte{},
			})
		}
		_, err = pool.UpdateDestChainConfig(deployer, laneConfigs)
		if err != nil {
			return cldf.ChangesetOutput{}, fmt.Errorf("failed to create updateLaneConfig transaction for %s token from %d: %w", c.TokenSymbol, sourceChainSelector, err)
		}
	}

	return deployerGroup.Enact()
}

func FastTransferUpdateFillerAllowlist(env cldf.Environment, c FastTransferFillerAllowlistConfig) (cldf.ChangesetOutput, error) {
	if err := c.Validate(env); err != nil {
		return cldf.ChangesetOutput{}, fmt.Errorf("invalid FastTransferFillerAllowlistConfig: %w", err)
	}

	state, err := stateview.LoadOnchainState(env)
	if err != nil {
		return cldf.ChangesetOutput{}, fmt.Errorf("failed to load onchain state: %w", err)
	}

	deployerGroup := deployergroup.NewDeployerGroup(env, state, c.MCMS).WithDeploymentContext(fmt.Sprintf("configure %s token pools", c.TokenSymbol))

	for sourceChainSelector, update := range c.Updates {
		deployer, err := deployerGroup.GetDeployer(sourceChainSelector)
		if err != nil {
			return cldf.ChangesetOutput{}, fmt.Errorf("failed to get deployer for chain selector %d: %w", sourceChainSelector, err)
		}

		pool, err := GetFastTransferTokenPoolContract(env, c.TokenSymbol, c.ContractType, c.ContractVersion, sourceChainSelector)
		if err != nil {
			return cldf.ChangesetOutput{}, fmt.Errorf("failed to get fast transfer token pool contract for %s token on chain %d: %w", c.TokenSymbol, sourceChainSelector, err)
		}

		_, err = pool.UpdateFillerAllowList(deployer, update.AddFillers, update.RemoveFillers)
		if err != nil {
			return cldf.ChangesetOutput{}, fmt.Errorf("failed to create updatefillerAllowList transaction for %s token from %d: %w", c.TokenSymbol, sourceChainSelector, err)
		}
	}

	return deployerGroup.Enact()

}
