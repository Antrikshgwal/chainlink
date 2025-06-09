package ccip

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/smartcontractkit/ccip-contract-examples-internal/gobindings/generated/latest/burn_mint_with_external_minter_fast_transfer_token_pool"
	stablecoin_utils "github.com/smartcontractkit/ccip-contract-examples-internal/gobindings/generated/latest/stablecoin_utils"
	"github.com/smartcontractkit/chainlink-ccip/chains/evm/gobindings/generated/latest/fast_transfer_token_pool_abstract"
	"github.com/smartcontractkit/chainlink-ccip/chains/evm/gobindings/generated/v1_5_1/token_pool"
	evmChain "github.com/smartcontractkit/chainlink-deployments-framework/chain/evm"
	"github.com/smartcontractkit/chainlink-deployments-framework/deployment"
	"github.com/smartcontractkit/chainlink-evm/gethwrappers/shared/generated/burn_mint_erc677"
	"github.com/smartcontractkit/chainlink-evm/gethwrappers/shared/generated/link_token"
	"github.com/smartcontractkit/chainlink-testing-framework/lib/blockchain"
	"github.com/smartcontractkit/chainlink-testing-framework/lib/logging"
	"github.com/smartcontractkit/chainlink/deployment/ccip/changeset/testhelpers"
	"github.com/smartcontractkit/chainlink/deployment/ccip/changeset/v1_5_1"
	"github.com/smartcontractkit/chainlink/deployment/ccip/shared"
	"github.com/smartcontractkit/chainlink/deployment/ccip/shared/stateview"
	"github.com/smartcontractkit/chainlink/deployment/ccip/shared/stateview/evm"

	"github.com/smartcontractkit/chainlink/deployment/environment/devenv"
	testsetups "github.com/smartcontractkit/chainlink/integration-tests/testsetups/ccip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	feeTokenLink   = "LINK"
	feeTokenNative = "NATIVE"
)

type balanceToken interface {
	BalanceOf(opts *bind.CallOpts, account common.Address) (*big.Int, error)
}

type balanceAssertion func(t *testing.T, sourceToken balanceToken, destinationToken balanceToken, address common.Address, description string)

type fastTransferE2ETestCase struct {
	name                                string
	enableFiller                        bool
	allowlistEnabled                    bool
	allowlistFiller                     bool
	tokenSymbol                         string
	preFastTransferFillerAssertions     []balanceAssertion
	postFastTransferFillerAssertions    []balanceAssertion
	postRegularTransferFillerAssertions []balanceAssertion
	preFastTransferUserAssertions       []balanceAssertion
	postFastTransferUserAssertions      []balanceAssertion
	postRegularTransferUserAssertions   []balanceAssertion
	preFastTransferPoolAssertions       []balanceAssertion
	postFastTransferPoolAssertions      []balanceAssertion
	postRegularTransferPoolAssertions   []balanceAssertion
	feeTokenType                        string // "LINK" or "NATIVE"
	fastTransferPoolFeeBps              uint16
	externalMinter                      bool
}

var (
	initialFillerTokenAmountOnDest = big.NewInt(0).Mul(big.NewInt(1e18), big.NewInt(1000))
	linkTokenAmount                = big.NewInt(0).Mul(big.NewInt(params.Ether), big.NewInt(1000))
	initialUserTokenAmountOnSource = big.NewInt(200000)
	defaultEthAmount               = big.NewInt(0).Mul(big.NewInt(params.Ether), big.NewInt(1000))
	transferAmount                 = big.NewInt(100000)
	expectedFastTransferFee        = big.NewInt(100)
	tokenDecimals                  = uint8(18)
	sourceChainId                  = uint64(1337)
	destinationChainId             = uint64(2337)
)

type fastTransferE2ETestCaseOption func(tc *fastTransferE2ETestCase) *fastTransferE2ETestCase

func ftfTc(name string, options ...fastTransferE2ETestCaseOption) *fastTransferE2ETestCase {
	tc := &fastTransferE2ETestCase{
		name:         name,
		enableFiller: true,
		preFastTransferFillerAssertions: []balanceAssertion{
			assertDestinationBalanceEqual(initialFillerTokenAmountOnDest),
		},
		preFastTransferUserAssertions: []balanceAssertion{
			assertSourceBalanceEqual(initialUserTokenAmountOnSource),
		},
		postFastTransferFillerAssertions:    []balanceAssertion{},
		postRegularTransferFillerAssertions: []balanceAssertion{},
		postFastTransferUserAssertions:      []balanceAssertion{},
		postRegularTransferUserAssertions:   []balanceAssertion{},
		preFastTransferPoolAssertions:       []balanceAssertion{},
		postFastTransferPoolAssertions:      []balanceAssertion{},
		postRegularTransferPoolAssertions:   []balanceAssertion{},
		feeTokenType:                        feeTokenLink,
		fastTransferPoolFeeBps:              0,
	}

	for _, option := range options {
		tc = option(tc)
	}

	return tc
}

func withFillerDisabled() fastTransferE2ETestCaseOption {
	return func(tc *fastTransferE2ETestCase) *fastTransferE2ETestCase {
		tc.enableFiller = false
		return tc
	}
}

func withFastFillSuccessAmountAssertions() fastTransferE2ETestCaseOption {
	transferAmountMinusFee := big.NewInt(0).Sub(transferAmount, expectedFastTransferFee)
	return func(tc *fastTransferE2ETestCase) *fastTransferE2ETestCase {
		// Calculate pool fee: (transferAmount * fastTransferPoolFeeBps) / 10000
		poolFee := big.NewInt(0).Mul(transferAmount, big.NewInt(int64(tc.fastTransferPoolFeeBps)))
		poolFee = big.NewInt(0).Div(poolFee, big.NewInt(10000))
		userReceivedAmount := big.NewInt(0).Sub(transferAmountMinusFee, poolFee)

		// Filler assertions
		tc.postRegularTransferFillerAssertions = append(tc.postRegularTransferFillerAssertions, assertDestinationBalanceEventuallyEqual(big.NewInt(0).Add(initialFillerTokenAmountOnDest, expectedFastTransferFee)))
		tc.postFastTransferFillerAssertions = append(tc.postFastTransferFillerAssertions, assertDestinationBalanceEventuallyEqual(big.NewInt(0).Sub(initialFillerTokenAmountOnDest, userReceivedAmount)))

		// User assertions
		tc.postFastTransferUserAssertions = append(tc.postFastTransferUserAssertions, assertDestinationBalanceEventuallyEqual(userReceivedAmount))
		tc.postRegularTransferUserAssertions = append(tc.postRegularTransferUserAssertions, assertDestinationBalanceEventuallyEqual(userReceivedAmount))

		// Pool assertions
		tc.preFastTransferPoolAssertions = append(tc.preFastTransferPoolAssertions, assertDestinationBalanceEqual(big.NewInt(0)))
		tc.postFastTransferPoolAssertions = append(tc.postFastTransferPoolAssertions, assertDestinationBalanceEventuallyEqual(big.NewInt(0)))
		tc.postRegularTransferPoolAssertions = append(tc.postRegularTransferPoolAssertions, assertDestinationBalanceEqual(poolFee))

		return tc
	}
}

func withFastFillNoFillerSuccessAmountAssertions() fastTransferE2ETestCaseOption {
	return func(tc *fastTransferE2ETestCase) *fastTransferE2ETestCase {
		// Filler assertions
		tc.postFastTransferFillerAssertions = append(tc.postFastTransferFillerAssertions, assertDestinationBalanceEqual(initialFillerTokenAmountOnDest))
		tc.postRegularTransferFillerAssertions = append(tc.postRegularTransferFillerAssertions, assertDestinationBalanceEqual(initialFillerTokenAmountOnDest))

		// User assertions
		tc.postFastTransferUserAssertions = append(tc.postFastTransferUserAssertions, assertDestinationBalanceEventuallyEqual(big.NewInt(0)))
		tc.postRegularTransferUserAssertions = append(tc.postRegularTransferUserAssertions, assertDestinationBalanceEventuallyEqual(transferAmount))

		// Pool assertions
		tc.preFastTransferPoolAssertions = append(tc.preFastTransferPoolAssertions, assertDestinationBalanceEqual(big.NewInt(0)))
		tc.postFastTransferPoolAssertions = append(tc.postFastTransferPoolAssertions, assertDestinationBalanceEventuallyEqual(big.NewInt(0)))
		tc.postRegularTransferPoolAssertions = append(tc.postRegularTransferPoolAssertions, assertDestinationBalanceEqual(big.NewInt(0)))

		return tc
	}
}

func withFeeTokenType(feeTokenType string) fastTransferE2ETestCaseOption {
	return func(tc *fastTransferE2ETestCase) *fastTransferE2ETestCase {
		tc.feeTokenType = feeTokenType
		return tc
	}
}

func withFillerAllowlistEnabled() fastTransferE2ETestCaseOption {
	return func(tc *fastTransferE2ETestCase) *fastTransferE2ETestCase {
		tc.allowlistEnabled = true
		return tc
	}
}

func withAllowlistFiller() fastTransferE2ETestCaseOption {
	return func(tc *fastTransferE2ETestCase) *fastTransferE2ETestCase {
		tc.allowlistFiller = true
		return tc
	}
}

func withPoolFeeBps(poolFeeBps uint16) fastTransferE2ETestCaseOption {
	return func(tc *fastTransferE2ETestCase) *fastTransferE2ETestCase {
		tc.fastTransferPoolFeeBps = poolFeeBps
		return tc
	}
}

func withExternalMinter() fastTransferE2ETestCaseOption {
	return func(tc *fastTransferE2ETestCase) *fastTransferE2ETestCase {
		tc.externalMinter = true
		return tc
	}
}

var fastTransferTestCases = []*fastTransferE2ETestCase{
	ftfTc("fee token", withFeeTokenType(feeTokenLink), withFastFillSuccessAmountAssertions()),
	ftfTc("fee token and no filler", withFeeTokenType(feeTokenLink), withFastFillNoFillerSuccessAmountAssertions(), withFillerDisabled()),
	ftfTc("native fee token", withFeeTokenType(feeTokenNative), withFastFillSuccessAmountAssertions()),
	ftfTc("native fee token and no filler", withFeeTokenType(feeTokenNative), withFastFillNoFillerSuccessAmountAssertions(), withFillerDisabled()),
	ftfTc("allowlist enabled", withFillerAllowlistEnabled(), withAllowlistFiller(), withFastFillSuccessAmountAssertions()),
	ftfTc("allowlist enabled and filler not on allowlist", withFillerAllowlistEnabled(), withFastFillNoFillerSuccessAmountAssertions()),
	ftfTc("pool fee with filler", withPoolFeeBps(50), withFastFillSuccessAmountAssertions()),
	ftfTc("pool fee without filler", withPoolFeeBps(50), withFastFillNoFillerSuccessAmountAssertions(), withFillerDisabled()),
	ftfTc("external minter", withExternalMinter(), withFastFillSuccessAmountAssertions(), withFeeTokenType(feeTokenNative)),
	ftfTc("external minter feeToken", withExternalMinter(), withFastFillSuccessAmountAssertions(), withFeeTokenType(feeTokenLink)),
}

func assertDestinationBalanceEventuallyEqual(expectedBalance *big.Int) balanceAssertion {
	return func(t *testing.T, sourceToken balanceToken, destinationToken balanceToken, address common.Address, description string) {
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			balance, err := destinationToken.BalanceOf(nil, address)
			require.NoError(collect, err)
			require.Equal(collect, expectedBalance.Int64(), balance.Int64(), "Balance should be equal to expected value")
		}, 30*time.Second, time.Second, fmt.Sprintf("%s - Balance should eventually be equal to expected value", description))
	}
}

func assertSourceBalanceEqual(expectedBalance *big.Int) balanceAssertion {
	return func(t *testing.T, sourceToken balanceToken, destinationToken balanceToken, address common.Address, descriptuon string) {
		balance, err := sourceToken.BalanceOf(nil, address)
		require.NoError(t, err)
		require.Equal(t, expectedBalance.Int64(), balance.Int64(), fmt.Sprintf("%s - Balance should be equal to expected value", descriptuon))
	}
}

func assertDestinationBalanceEqual(expectedBalance *big.Int) balanceAssertion {
	return func(t *testing.T, sourceToken balanceToken, destinationToken balanceToken, address common.Address, description string) {
		balance, err := destinationToken.BalanceOf(nil, address)
		require.NoError(t, err)
		require.Equal(t, expectedBalance.Int64(), balance.Int64(), fmt.Sprintf("%s - Balance should be equal to expected value", description))
	}
}

func createAccount(t *testing.T, chainId uint64) (common.Address, func() *bind.TransactOpts, *ecdsa.PrivateKey) {
	userPrivateKey, err := crypto.GenerateKey()
	require.NoError(t, err)
	userAddress := crypto.PubkeyToAddress(userPrivateKey.PublicKey)
	transactor := func() *bind.TransactOpts {
		userTransactor, err := bind.NewKeyedTransactorWithChainID(userPrivateKey, big.NewInt(int64(chainId)))
		require.NoError(t, err)
		return userTransactor
	}
	return userAddress, transactor, userPrivateKey
}

func deployTokenAndGrantAllRoles(t *testing.T, chain evmChain.Chain, tokenSymbol string, tokenDecimals uint8, lock *sync.Mutex, isExternalMinterToken bool) token {
	lock.Lock()
	defer lock.Unlock()

	if isExternalMinterToken {
		_, tx, token, err := stablecoin_utils.DeployStablecoin(
			chain.DeployerKey,
			chain.Client,
		)
		require.NoError(t, err)
		_, err = chain.Confirm(tx)
		require.NoError(t, err)

		tx, err = token.Initialize(chain.DeployerKey, tokenSymbol, tokenSymbol)
		require.NoError(t, err)
		_, err = chain.Confirm(tx)
		require.NoError(t, err)

		return token
	}

	_, tx, token, err := burn_mint_erc677.DeployBurnMintERC677(
		chain.DeployerKey,
		chain.Client,
		tokenSymbol,
		tokenSymbol,
		tokenDecimals,
		big.NewInt(0).Mul(big.NewInt(1e9), big.NewInt(1e18)),
	)
	require.NoError(t, err)
	_, err = chain.Confirm(tx)
	require.NoError(t, err)

	tx, err = token.GrantMintAndBurnRoles(chain.DeployerKey, chain.DeployerKey.From)
	require.NoError(t, err)
	_, err = chain.Confirm(tx)
	require.NoError(t, err)

	return token
}

func getLinkTokenAndGrantMintRole(t *testing.T, chain evmChain.Chain, state evm.CCIPChainState, sendLock *sync.Mutex) *link_token.LinkToken {
	sendLock.Lock()
	defer sendLock.Unlock()
	linkToken := state.LinkToken
	tx, err := linkToken.GrantMintRole(chain.DeployerKey, chain.DeployerKey.From)
	require.NoError(t, err)
	_, err = chain.Confirm(tx)
	require.NoError(t, err)

	return linkToken
}

type mintableToken interface {
	Mint(opts *bind.TransactOpts, account common.Address, amount *big.Int) (*types.Transaction, error)
	Address() common.Address
}

type token interface {
	balanceToken
	approvableToken
	Address() common.Address
}

func fundAccountWithToken(t *testing.T, chain evmChain.Chain, receiver common.Address, token mintableToken, amount *big.Int, sendLock *sync.Mutex) {
	sendLock.Lock()
	defer sendLock.Unlock()
	tx, err := token.Mint(chain.DeployerKey, receiver, amount)
	require.NoError(t, err)
	_, err = chain.Confirm(tx)
	require.NoError(t, err)
}

type approvableToken interface {
	Approve(opts *bind.TransactOpts, spender common.Address, amount *big.Int) (*types.Transaction, error)
}

func approveToken(t *testing.T, chain evmChain.Chain, transactor *bind.TransactOpts, token approvableToken, spender common.Address) {
	tx, err := token.Approve(transactor, spender, big.NewInt(0).Mul(big.NewInt(1e18), big.NewInt(1e9))) // Approve a large amount
	require.NoError(t, err)
	_, err = chain.Confirm(tx)
	require.NoError(t, err)
}

type tokenPoolConfig struct {
	poolConfig        map[uint64]v1_5_1.DeployTokenPoolInput
	sourceMinter      mintableToken
	destinationMinter mintableToken
	postSetupAction   func(sourceTokenPool common.Address, destinationTokenPool common.Address)
	version           semver.Version
	poolType          deployment.ContractType
}

func configureExternalMinterTokenPool(t *testing.T, e deployment.Environment, sourceChainSelector, destinationChainSelector uint64, sourceTokenAddress, destinationTokenAddress common.Address, tokenDecimals uint8) tokenPoolConfig {
	sourceChain := e.BlockChains.EVMChains()[sourceChainSelector]
	destChain := e.BlockChains.EVMChains()[destinationChainSelector]

	_, sourceTokenGovernor := testhelpers.DeployTokenGovernor(t, e, sourceChainSelector, sourceTokenAddress)
	_, destinationTokenGovernor := testhelpers.DeployTokenGovernor(t, e, destinationChainSelector, destinationTokenAddress)

	bridgeBurnMintRole, err := sourceTokenGovernor.BRIDGEMINTERORBURNERROLE(nil)
	require.NoError(t, err)

	poolConfig := map[uint64]v1_5_1.DeployTokenPoolInput{
		sourceChainSelector: {
			Type:               shared.BurnMintWithExternalMinterFastTransferTokenPool,
			TokenAddress:       sourceTokenAddress,
			AllowList:          nil,
			LocalTokenDecimals: tokenDecimals,
			AcceptLiquidity:    nil,
			ExternalMinter:     sourceTokenGovernor.Address(),
		},
		destinationChainSelector: {
			Type:               shared.BurnMintWithExternalMinterFastTransferTokenPool,
			TokenAddress:       destinationTokenAddress,
			AllowList:          nil,
			LocalTokenDecimals: tokenDecimals,
			AcceptLiquidity:    nil,
			ExternalMinter:     destinationTokenGovernor.Address(),
		},
	}

	postSetupAction := func(sourceTokenPool common.Address, destinationTokenPool common.Address) {
		tx, err := sourceTokenGovernor.GrantRole(sourceChain.DeployerKey, bridgeBurnMintRole, sourceTokenPool)
		require.NoError(t, err)
		_, err = sourceChain.Confirm(tx)
		require.NoError(t, err)
		tx, err = destinationTokenGovernor.GrantRole(destChain.DeployerKey, bridgeBurnMintRole, destinationTokenPool)
		require.NoError(t, err)
		_, err = destChain.Confirm(tx)
		require.NoError(t, err)

		sourceToken, err := stablecoin_utils.NewStablecoin(sourceTokenAddress, sourceChain.Client)
		require.NoError(t, err)
		tx, err = sourceToken.TransferOwnership(sourceChain.DeployerKey, sourceTokenGovernor.Address())
		require.NoError(t, err)
		_, err = sourceChain.Confirm(tx)
		require.NoError(t, err)

		tx, err = sourceTokenGovernor.AcceptOwnership(sourceChain.DeployerKey)
		require.NoError(t, err)
		_, err = sourceChain.Confirm(tx)
		require.NoError(t, err)

		destinationToken, err := stablecoin_utils.NewStablecoin(destinationTokenAddress, destChain.Client)
		require.NoError(t, err)
		tx, err = destinationToken.TransferOwnership(destChain.DeployerKey, destinationTokenGovernor.Address())
		require.NoError(t, err)
		_, err = destChain.Confirm(tx)
		require.NoError(t, err)
		tx, err = destinationTokenGovernor.AcceptOwnership(destChain.DeployerKey)
		require.NoError(t, err)
		_, err = destChain.Confirm(tx)
		require.NoError(t, err)

		minterRole, err := sourceTokenGovernor.MINTERROLE(nil)
		require.NoError(t, err)
		tx, err = sourceTokenGovernor.GrantRole(sourceChain.DeployerKey, minterRole, sourceChain.DeployerKey.From)
		require.NoError(t, err)
		_, err = sourceChain.Confirm(tx)
		require.NoError(t, err)
		tx, err = destinationTokenGovernor.GrantRole(destChain.DeployerKey, minterRole, destChain.DeployerKey.From)
		require.NoError(t, err)
		_, err = destChain.Confirm(tx)
		require.NoError(t, err)
	}

	return tokenPoolConfig{
		poolConfig:        poolConfig,
		sourceMinter:      sourceTokenGovernor,
		destinationMinter: destinationTokenGovernor,
		postSetupAction:   postSetupAction,
		version:           shared.BurnMintWithExternalMinterFastTransferTokenPoolVersion,
		poolType:          shared.BurnMintWithExternalMinterFastTransferTokenPool,
	}
}

func configureBurnMintTokenPool(t *testing.T, e deployment.Environment, sourceChainSelector, destinationChainSelector uint64, sourceTokenAddress, destinationTokenAddress common.Address, tokenDecimals uint8) tokenPoolConfig {
	sourceChain := e.BlockChains.EVMChains()[sourceChainSelector]
	destChain := e.BlockChains.EVMChains()[destinationChainSelector]

	poolConfig := map[uint64]v1_5_1.DeployTokenPoolInput{
		sourceChainSelector: {
			Type:               shared.BurnMintFastTransferTokenPool,
			TokenAddress:       sourceTokenAddress,
			AllowList:          nil,
			LocalTokenDecimals: tokenDecimals,
			AcceptLiquidity:    nil,
		},
		destinationChainSelector: {
			Type:               shared.BurnMintFastTransferTokenPool,
			TokenAddress:       destinationTokenAddress,
			AllowList:          nil,
			LocalTokenDecimals: tokenDecimals,
			AcceptLiquidity:    nil,
		},
	}

	sourceToken, err := burn_mint_erc677.NewBurnMintERC677(sourceTokenAddress, sourceChain.Client)
	require.NoError(t, err)
	destToken, err := burn_mint_erc677.NewBurnMintERC677(destinationTokenAddress, destChain.Client)
	require.NoError(t, err)

	postSetupAction := func(sourceTokenPool common.Address, destinationTokenPool common.Address) {
		sourceToken, err := burn_mint_erc677.NewBurnMintERC677(sourceTokenAddress, sourceChain.Client)
		require.NoError(t, err)
		tx, err := sourceToken.GrantBurnRole(sourceChain.DeployerKey, sourceTokenPool)
		require.NoError(t, err)
		_, err = sourceChain.Confirm(tx)
		require.NoError(t, err)

		tx, err = destToken.GrantMintRole(destChain.DeployerKey, destinationTokenPool)
		require.NoError(t, err)
		_, err = destChain.Confirm(tx)
		require.NoError(t, err)
	}

	return tokenPoolConfig{
		poolConfig:        poolConfig,
		sourceMinter:      sourceToken,
		destinationMinter: destToken,
		postSetupAction:   postSetupAction,
		version:           shared.FastTransferTokenPoolVersion,
		poolType:          shared.BurnMintFastTransferTokenPool,
	}
}

func configureTokenPoolRateLimits(e deployment.Environment, tokenSymbol string, sourceChainSelector, destinationChainSelector uint64, poolType deployment.ContractType, version semver.Version) error {
	ratelimiterConfig := token_pool.RateLimiterConfig{
		IsEnabled: true,
		Capacity:  new(big.Int).Mul(big.NewInt(1e16), big.NewInt(2)),
		Rate:      big.NewInt(1),
	}
	tokenPoolConfig := map[uint64]v1_5_1.TokenPoolConfig{
		sourceChainSelector: {
			Type:    poolType,
			Version: version,
			ChainUpdates: v1_5_1.RateLimiterPerChain{
				destinationChainSelector: v1_5_1.RateLimiterConfig{
					Inbound:  ratelimiterConfig,
					Outbound: ratelimiterConfig,
				},
			},
		},
		destinationChainSelector: {
			Type:    poolType,
			Version: version,
			ChainUpdates: v1_5_1.RateLimiterPerChain{
				sourceChainSelector: v1_5_1.RateLimiterConfig{
					Inbound:  ratelimiterConfig,
					Outbound: ratelimiterConfig,
				},
			},
		},
	}
	_, err := v1_5_1.ConfigureTokenPoolContractsChangeset(e, v1_5_1.ConfigureTokenPoolContractsConfig{
		TokenSymbol: shared.TokenSymbol(tokenSymbol),
		PoolUpdates: tokenPoolConfig,
	})
	return err
}

func configureTokenAdminRegistry(e deployment.Environment, tokenSymbol string, sourceChainSelector, destinationChainSelector uint64, poolType deployment.ContractType, version semver.Version) error {
	registryConfig := map[uint64]map[shared.TokenSymbol]v1_5_1.TokenPoolInfo{
		sourceChainSelector: {
			shared.TokenSymbol(tokenSymbol): {
				Type:          poolType,
				Version:       version,
				ExternalAdmin: e.BlockChains.EVMChains()[sourceChainSelector].DeployerKey.From,
			},
		},
		destinationChainSelector: {
			shared.TokenSymbol(tokenSymbol): {
				Type:          poolType,
				Version:       version,
				ExternalAdmin: e.BlockChains.EVMChains()[destinationChainSelector].DeployerKey.From,
			},
		},
	}

	_, err := v1_5_1.ProposeAdminRoleChangeset(e, v1_5_1.TokenAdminRegistryChangesetConfig{
		Pools:                   registryConfig,
		SkipOwnershipValidation: true,
	})
	if err != nil {
		return err
	}

	_, err = v1_5_1.AcceptAdminRoleChangeset(e, v1_5_1.TokenAdminRegistryChangesetConfig{
		Pools:                   registryConfig,
		SkipOwnershipValidation: true,
	})
	if err != nil {
		return err
	}

	_, err = v1_5_1.SetPoolChangeset(e, v1_5_1.TokenAdminRegistryChangesetConfig{
		Pools:                   registryConfig,
		SkipOwnershipValidation: true,
	})
	return err
}

func getFirstAddressFromChain(t *testing.T, addressBook deployment.AddressBook, chainSelector uint64) common.Address {
	addresses, err := addressBook.AddressesForChain(chainSelector)
	require.NoError(t, err)

	for addr, _ := range addresses {
		return common.HexToAddress(addr)
	}

	require.Failf(t, "No addresses found for chain", "ChainSelector: %d", chainSelector)
	return common.Address{}
}

func configureFastTransferSettings(e deployment.Environment, tokenSymbol string, sourceChainSelector, destinationChainSelector uint64, fillerAddress common.Address, tc *fastTransferE2ETestCase, poolType deployment.ContractType, version semver.Version) error {
	fillers := []common.Address{}
	if tc.allowlistEnabled && tc.allowlistFiller {
		fillers = append(fillers, fillerAddress)
	}

	if tc.allowlistFiller {
		_, err := v1_5_1.FastTransferUpdateFillerAllowlist(e, v1_5_1.FastTransferFillerAllowlistConfig{
			TokenSymbol:     shared.TokenSymbol(tokenSymbol),
			ContractType:    poolType,
			ContractVersion: version,
			Updates: map[uint64]v1_5_1.FillerAllowlistConfig{
				sourceChainSelector: {
					AddFillers:    fillers,
					RemoveFillers: []common.Address{},
				},
				destinationChainSelector: {
					AddFillers:    fillers,
					RemoveFillers: []common.Address{},
				},
			},
		})
		if err != nil {
			return err
		}
	}

	_, err := v1_5_1.FastTransferUpdateLaneConfigChangeset(e, v1_5_1.FastTransferUpdateLaneConfigConfig{
		TokenSymbol:     shared.TokenSymbol(tokenSymbol),
		ContractType:    poolType,
		ContractVersion: version,
		Updates: map[uint64](map[uint64]v1_5_1.UpdateLaneConfig){
			sourceChainSelector: {
				destinationChainSelector: {
					FastTransferFillerFeeBps: 10,
					FastTransferPoolFeeBps:   tc.fastTransferPoolFeeBps,
					FillerAllowlistEnabled:   tc.allowlistEnabled,
					FillAmountMaxRequest:     big.NewInt(100000),
					SkipAllowlistValidation:  true,
				},
			},
			destinationChainSelector: {
				sourceChainSelector: {
					FastTransferFillerFeeBps: 20,
					FastTransferPoolFeeBps:   tc.fastTransferPoolFeeBps,
					FillerAllowlistEnabled:   tc.allowlistEnabled,
					FillAmountMaxRequest:     big.NewInt(100000),
					SkipAllowlistValidation:  true,
				},
			},
		},
	})
	return err
}

func configureTokenPoolContracts(t *testing.T, e deployment.Environment, tokenSymbol string, sourceChainSelector, destinationChainSelector uint64, sourceTokenAddress, destinationTokenAddress common.Address, tokenDecimals uint8, fillerAddress common.Address, tc *fastTransferE2ETestCase, sourceLock *sync.Mutex, destinationLock *sync.Mutex) (common.Address, common.Address, semver.Version, *fast_transfer_token_pool_abstract.FastTransferTokenPoolAbstract, mintableToken, mintableToken) {
	sourceLock.Lock()
	defer sourceLock.Unlock()
	destinationLock.Lock()
	defer destinationLock.Unlock()

	var config tokenPoolConfig
	if tc.externalMinter {
		config = configureExternalMinterTokenPool(t, e, sourceChainSelector, destinationChainSelector, sourceTokenAddress, destinationTokenAddress, tokenDecimals)
	} else {
		config = configureBurnMintTokenPool(t, e, sourceChainSelector, destinationChainSelector, sourceTokenAddress, destinationTokenAddress, tokenDecimals)
	}

	cs, err := v1_5_1.DeployTokenPoolContractsChangeset(e, v1_5_1.DeployTokenPoolContractsConfig{
		TokenSymbol: shared.TokenSymbol(tokenSymbol),
		NewPools:    config.poolConfig,
	})
	require.NoError(t, err)

	sourceTokenPoolAddress := getFirstAddressFromChain(t, cs.AddressBook, sourceChainSelector)
	destinationTokenPoolAddress := getFirstAddressFromChain(t, cs.AddressBook, destinationChainSelector)

	err = e.ExistingAddresses.Merge(cs.AddressBook)
	require.NoError(t, err)

	err = configureTokenPoolRateLimits(e, tokenSymbol, sourceChainSelector, destinationChainSelector, config.poolType, config.version)
	require.NoError(t, err)

	err = configureTokenAdminRegistry(e, tokenSymbol, sourceChainSelector, destinationChainSelector, config.poolType, config.version)
	require.NoError(t, err)

	err = configureFastTransferSettings(e, tokenSymbol, sourceChainSelector, destinationChainSelector, fillerAddress, tc, config.poolType, config.version)
	require.NoError(t, err)

	sourceTokenPool, err := v1_5_1.GetFastTransferTokenPoolContract(e, shared.TokenSymbol(tokenSymbol), config.poolType, config.version, sourceChainSelector)
	require.NoError(t, err)

	config.postSetupAction(sourceTokenPoolAddress, destinationTokenPoolAddress)

	return sourceTokenPoolAddress, destinationTokenPoolAddress, config.version, sourceTokenPool, config.sourceMinter, config.destinationMinter
}

func runAssertions(t *testing.T, sourceToken balanceToken, destinationToken balanceToken, address common.Address, assertions []balanceAssertion, description string) {
	for _, assertion := range assertions {
		assertion(t, sourceToken, destinationToken, address, description)
	}
}

func startRelayer(t *testing.T, sourceChainSelector, destinationChainSelector uint64, sourceTokenPoolAddress common.Address, destinationTokenPoolAddress common.Address, deployedEnv testhelpers.TestEnvironment, fillerPrivateKey *ecdsa.PrivateKey) func() error {
	dockerEnv, ok := deployedEnv.(*testsetups.DeployedLocalDevEnvironment)
	require.True(t, ok, "deployedEnv is not of type *testsetups.DeployedLocalDevEnvironment")

	networks := dockerEnv.GetCLClusterTestEnv().EVMNetworks
	var sourceChainNetwork *blockchain.EVMNetwork
	for _, network := range networks {
		if uint64(network.ChainID) == sourceChainId {
			sourceChainNetwork = network
			break
		}
	}
	require.NotNil(t, sourceChainNetwork, "Source chain network not found in EVM networks")

	var destinationChainNetwork *blockchain.EVMNetwork
	for _, network := range networks {
		if uint64(network.ChainID) == destinationChainId {
			destinationChainNetwork = network
			break
		}
	}
	require.NotNil(t, destinationChainNetwork, "Destination chain network not found in EVM networks")

	marshalledKey := crypto.FromECDSA(fillerPrivateKey)

	hexString := hex.EncodeToString(marshalledKey)

	fastFillerConfig := devenv.CCIPFastFillerConfig{
		SignerProviders: []devenv.SignerProvider{
			{
				Name:       "filler",
				Type:       "raw",
				PrivateKey: hexString,
			},
		},
		Listeners: []devenv.ListenerConfig{
			{
				ChainSelector:    strconv.FormatUint(sourceChainSelector, 10),
				TokenPoolAddress: sourceTokenPoolAddress.Hex(),
				RpcURL:           sourceChainNetwork.HTTPURLs[0],
			},
		},
		Fillers: []devenv.FillerConfig{
			{
				ChainSelector:    strconv.FormatUint(destinationChainSelector, 10),
				TokenPoolAddress: destinationTokenPoolAddress.Hex(),
				RpcURL:           destinationChainNetwork.HTTPURLs[0],
				SignerProvider:   "filler",
			},
		},
	}
	l := logging.GetTestLogger(t)
	relayer := devenv.NewCCIPFastFiller(fastFillerConfig, l, []string{dockerEnv.GetCLClusterTestEnv().DockerNetwork.ID})
	err := relayer.Start(t.Context(), t)
	require.NoError(t, err, "Failed to start the relayer")

	return func() error { return relayer.Stop(context.Background()) }
}

func TestSimpleFastTransfer(t *testing.T) {
	e, _, deployedEnv := testsetups.NewIntegrationEnvironment(t)

	onChainState, err := stateview.LoadOnchainState(e.Env)
	require.NoError(t, err)
	testhelpers.AddLanesForAll(t, &e, onChainState)

	sourceChainSelector := e.Env.BlockChains.ListChainSelectors()[0]
	destinationChainSelector := e.Env.BlockChains.ListChainSelectors()[1]

	sourceChain := e.Env.BlockChains.EVMChains()[sourceChainSelector]
	sourceChainState := onChainState.Chains[sourceChainSelector]
	destinationChain := e.Env.BlockChains.EVMChains()[destinationChainSelector]

	sourceLock := sync.Mutex{}
	destinationLock := sync.Mutex{}
	sendLock := sync.Mutex{}

	for i, tc := range fastTransferTestCases {
		tc.tokenSymbol = fmt.Sprintf("FTF_TEST_%d", i+1)
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			userAddress, userTransactor, _ := createAccount(t, sourceChainId)
			fillerAddress, fillerTransactor, fillerPrivateKey := createAccount(t, destinationChainId)
			sourceToken := deployTokenAndGrantAllRoles(t, sourceChain, tc.tokenSymbol, tokenDecimals, &sourceLock, tc.externalMinter)
			destinationToken := deployTokenAndGrantAllRoles(t, destinationChain, tc.tokenSymbol, tokenDecimals, &destinationLock, tc.externalMinter)

			sourceTokenPoolAddress, destinationTokenPoolAddress, _, _, sourceMinter, destinationMinter := configureTokenPoolContracts(t, e.Env, tc.tokenSymbol, sourceChainSelector, destinationChainSelector, sourceToken.Address(), destinationToken.Address(), tokenDecimals, fillerAddress, tc, &sourceLock, &destinationLock)
			srcChainConfigDone := make(chan struct{})
			destinationChainConfigDone := make(chan struct{})
			go func() {
				fundAccount(t, sourceChain, userAddress, defaultEthAmount, &sourceLock)
				fundAccountWithToken(t, sourceChain, userAddress, sourceMinter, initialUserTokenAmountOnSource, &sourceLock)

				approveToken(t, sourceChain, userTransactor(), sourceToken, sourceTokenPoolAddress)

				switch tc.feeTokenType {
				case feeTokenLink:
					sourceLinkToken := getLinkTokenAndGrantMintRole(t, sourceChain, sourceChainState, &sourceLock)
					fundAccountWithToken(t, sourceChain, userAddress, sourceLinkToken, linkTokenAmount, &sourceLock)
					approveToken(t, sourceChain, userTransactor(), sourceLinkToken, sourceTokenPoolAddress)
				case feeTokenNative:
					approveToken(t, sourceChain, userTransactor(), sourceChainState.Weth9, sourceChainState.Router.Address())
				}

				close(srcChainConfigDone)
			}()

			go func() {
				fundAccount(t, destinationChain, fillerAddress, defaultEthAmount, &destinationLock)
				fundAccountWithToken(t, destinationChain, fillerAddress, destinationMinter, initialFillerTokenAmountOnDest, &destinationLock)
				approveToken(t, destinationChain, fillerTransactor(), destinationToken, destinationTokenPoolAddress)

				close(destinationChainConfigDone)
			}()

			onChainState, err = stateview.LoadOnchainState(e.Env)
			require.NoError(t, err)

			if tc.enableFiller {
				stop := startRelayer(t, sourceChainSelector, destinationChainSelector, sourceTokenPoolAddress, destinationTokenPoolAddress, deployedEnv, fillerPrivateKey)
				e.Env.Logger.Infof("Started relayer for source chain %d and destination chain %d", sourceChainSelector, destinationChainSelector)

				defer func() {
					e.Env.Logger.Infof("Stopping relayer for source chain %d and destination chain %d", sourceChainSelector, destinationChainSelector)
					stop()
				}()
			}

			<-chan struct{}(srcChainConfigDone)
			<-chan struct{}(destinationChainConfigDone)

			runAssertions(t, sourceToken, destinationToken, fillerAddress, tc.preFastTransferFillerAssertions, "Pre Fast Transfer Filler Assertions")
			runAssertions(t, sourceToken, destinationToken, userAddress, tc.preFastTransferUserAssertions, "Pre Fast Transfer User Assertions")
			runAssertions(t, sourceToken, destinationToken, destinationTokenPoolAddress, tc.preFastTransferPoolAssertions, "Pre Fast Transfer Pool Assertions")

			userTransac := userTransactor()
			userTransac.GasLimit = 1000000
			e.Env.Logger.Infof("Sending transaction from user address: %s", userTransac.From.Hex())

			var feeTokenAddress common.Address
			if tc.feeTokenType == feeTokenLink {
				feeTokenAddress = onChainState.Chains[sourceChainSelector].LinkToken.Address()
			} else if tc.feeTokenType == feeTokenNative {
				userTransac.Value = big.NewInt(0).Mul(big.NewInt(params.Ether), big.NewInt(100))
				feeTokenAddress = common.HexToAddress("0x0")
			} else {
				t.Fatalf("Unknown fee token type: %s", tc.feeTokenType)
			}

			var seqNum uint64
			func() {
				sendLock.Lock()
				defer sendLock.Unlock()
				pool, err := burn_mint_with_external_minter_fast_transfer_token_pool.NewBurnMintWithExternalMinterFastTransferTokenPool(sourceTokenPoolAddress, sourceChain.Client)
				require.NoError(t, err)
				seqNum, err = onChainState.Chains[sourceChainSelector].OnRamp.GetExpectedNextSequenceNumber(nil, destinationChainSelector)
				require.NoError(t, err)
				e.Env.Logger.Infof("Sending with user: %s", userTransac.From.Hex())
				tx, err := pool.CcipSendToken(userTransac, destinationChainSelector, transferAmount, common.LeftPadBytes(userAddress.Bytes(), 32), feeTokenAddress, []byte{})
				txJSON, _ := tx.MarshalJSON()
				e.Env.Logger.Infof("Transaction JSON: %s", string(txJSON))
				e.Env.Logger.Infof("Sending transaction: %s", tx.Hash().Hex())
				require.NoError(t, err)
				_, err = sourceChain.Confirm(tx)
				require.NoError(t, err)
			}()

			runAssertions(t, sourceToken, destinationToken, fillerAddress, tc.postFastTransferFillerAssertions, "Post Fast Transfer Filler Assertions")
			runAssertions(t, sourceToken, destinationToken, userAddress, tc.postFastTransferUserAssertions, "Post Fast Transfer User Assertions")
			runAssertions(t, sourceToken, destinationToken, destinationTokenPoolAddress, tc.postFastTransferPoolAssertions, "Post Fast Transfer Pool Assertions")

			zero := uint64(0)
			testhelpers.ConfirmExecWithSeqNrsForAll(t, e.Env, onChainState, map[testhelpers.SourceDestPair][]uint64{
				{
					SourceChainSelector: sourceChainSelector,
					DestChainSelector:   destinationChainSelector,
				}: {uint64(seqNum)},
			}, map[uint64]*uint64{
				destinationChainSelector: &zero,
			})

			runAssertions(t, sourceToken, destinationToken, fillerAddress, tc.postRegularTransferFillerAssertions, "Post Regular Transfer Filler Assertions")
			runAssertions(t, sourceToken, destinationToken, userAddress, tc.postRegularTransferUserAssertions, "Post Regular Transfer User Assertions")
			runAssertions(t, sourceToken, destinationToken, destinationTokenPoolAddress, tc.postRegularTransferPoolAssertions, "Post Regular Transfer Pool Assertions")
		})
	}
}

func fundAccount(
	t *testing.T,
	chain evmChain.Chain,
	receiver common.Address,
	amount *big.Int,
	sendLock *sync.Mutex,
) {
	sendLock.Lock()
	defer sendLock.Unlock()
	client := chain.Client
	sender := chain.DeployerKey

	nonce, err := client.NonceAt(t.Context(), sender.From, nil)
	require.NoError(t, err)

	gasPrice, err := client.SuggestGasPrice(t.Context())
	require.NoError(t, err)
	gasLimit := uint64(21000)

	tx := types.NewTransaction(nonce, receiver, amount, gasLimit, gasPrice, nil)

	signedTx, err := sender.Signer(sender.From, tx)
	require.NoError(t, err)

	err = client.SendTransaction(t.Context(), signedTx)
	require.NoError(t, err)

	_, err = chain.Confirm(signedTx)
	require.NoError(t, err)
}
