package app

import (
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/cosmos/cosmos-sdk/x/auth"
	sdkbank "github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/crisis"
	distr "github.com/cosmos/cosmos-sdk/x/distribution"
	"github.com/cosmos/cosmos-sdk/x/evidence"
	"github.com/cosmos/cosmos-sdk/x/genutil"
	"github.com/cosmos/cosmos-sdk/x/gov"
	"github.com/cosmos/cosmos-sdk/x/mint"
	"github.com/cosmos/cosmos-sdk/x/params"
	paramsclient "github.com/cosmos/cosmos-sdk/x/params/client"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	"github.com/cosmos/cosmos-sdk/x/staking"
	"github.com/cosmos/cosmos-sdk/x/supply"
	"github.com/cosmos/cosmos-sdk/x/upgrade"
	upgradeclient "github.com/cosmos/cosmos-sdk/x/upgrade/client"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/abci/version"
	"github.com/tendermint/tendermint/libs/log"
	tmos "github.com/tendermint/tendermint/libs/os"
	dbm "github.com/tendermint/tm-db"

	"github.com/cybercongress/cyberd/store"
	"github.com/cybercongress/cyberd/types"
	"github.com/cybercongress/cyberd/types/coin"
	"github.com/cybercongress/cyberd/util"
	bandwidth "github.com/cybercongress/cyberd/x/bandwidth"
	cyberbank "github.com/cybercongress/cyberd/x/bank"
	"github.com/cybercongress/cyberd/x/link"
	"github.com/cybercongress/cyberd/x/rank"
)

const (
	appName        = "CyberApp"
)

// default home directories for expected binaries
var (
	DefaultCLIHome  = os.ExpandEnv("$HOME/.cybercli")
	DefaultNodeHome = os.ExpandEnv("$HOME/.cyberd")

	ModuleBasics = module.NewBasicManager(
		auth.AppModuleBasic{},
		sdkbank.AppModuleBasic{},
		staking.AppModuleBasic{},
		mint.AppModuleBasic{},
		distr.AppModuleBasic{},
		gov.NewAppModuleBasic(paramsclient.ProposalHandler, distr.ProposalHandler, upgradeclient.ProposalHandler),
		params.AppModuleBasic{},
		crisis.AppModuleBasic{},
		slashing.AppModuleBasic{},
		supply.AppModuleBasic{},

		link.AppModuleBasic{},
		bandwidth.AppModuleBasic{},
		rank.AppModuleBasic{},
	)

	maccPerms = map[string][]string{
		auth.FeeCollectorName:     nil,
		distr.ModuleName:          nil,
		mint.ModuleName:           {supply.Minter},
		staking.BondedPoolName:    {supply.Burner, supply.Staking},
		staking.NotBondedPoolName: {supply.Burner, supply.Staking},
		gov.ModuleName:            {supply.Burner},
	}
)

// CyberdApp implements an extended ABCI application. It contains a BaseApp,
// a codec for serialization, KVStore dbKeys for multistore state management, and
// various mappers and keepers to manage getting, setting, and serializing the
// integral app types.
type CyberdApp struct {
	*baseapp.BaseApp
	cdc *codec.Codec

	txDecoder      sdk.TxDecoder
	invCheckPeriod uint

	bandwidthMeter       bandwidth.Meter
	blockBandwidthKeeper bandwidth.BlockSpentBandwidthKeeper

	dbKeys cyberdAppDbKeys

	subspaces map[string]params.Subspace

	mainKeeper         store.MainKeeper
	accountKeeper      auth.AccountKeeper
	supplyKeeper       supply.Keeper
	stakingKeeper      staking.Keeper
	slashingKeeper     slashing.Keeper
	mintKeeper         mint.Keeper
	distrKeeper        distr.Keeper
	govKeeper          gov.Keeper
	paramsKeeper       params.Keeper
	crisisKeeper       crisis.Keeper
	upgradeKeeper      upgrade.Keeper
	evidenceKeeper     evidence.Keeper

	bankKeeper         cyberbank.Keeper
	accBandwidthKeeper bandwidth.Keeper
	linkIndexedKeeper  link.IndexedKeeper
	cidNumKeeper       link.CidNumberKeeper
	stakingIndexKeeper cyberbank.IndexedKeeper
	rankStateKeeper    rank.StateKeeper

	latestBlockHeight int64

	mm *module.Manager
}

func NewCyberdApp(logger log.Logger, db dbm.DB, traceStore io.Writer, loadLatest bool,
	invCheckPeriod uint, skipUpgradeHeights map[int64]bool,
	computeUnit rank.ComputeUnit, allowSearch bool,
	baseAppOptions ...func(*baseapp.BaseApp),
) *CyberdApp {

	cdc := MakeCodec()
	txDecoder := auth.DefaultTxDecoder(cdc)
	baseApp := baseapp.NewBaseApp(appName, logger, db, txDecoder, baseAppOptions...)
	baseApp.SetCommitMultiStoreTracer(traceStore)
	dbKeys := NewCyberdAppDbKeys()
	mainKeeper := store.NewMainKeeper(dbKeys.main)
	baseApp.SetAppVersion(version.Version)

	var app = &CyberdApp{
		BaseApp:        baseApp,
		cdc:            cdc,
		txDecoder:      txDecoder,
		invCheckPeriod: invCheckPeriod,
		dbKeys:         dbKeys,
		mainKeeper:     mainKeeper,
		subspaces:      make(map[string]params.Subspace),
	}

	app.paramsKeeper = params.NewKeeper(app.cdc, dbKeys.params, dbKeys.tParams)
	app.subspaces[auth.ModuleName] = app.paramsKeeper.Subspace(auth.DefaultParamspace)
	app.subspaces[sdkbank.ModuleName] = app.paramsKeeper.Subspace(sdkbank.DefaultParamspace)
	app.subspaces[staking.ModuleName] = app.paramsKeeper.Subspace(staking.DefaultParamspace)
	app.subspaces[slashing.ModuleName] = app.paramsKeeper.Subspace(slashing.DefaultParamspace)
	app.subspaces[mint.ModuleName] = app.paramsKeeper.Subspace(mint.DefaultParamspace)
	app.subspaces[distr.ModuleName] = app.paramsKeeper.Subspace(distr.DefaultParamspace)
	app.subspaces[crisis.ModuleName] = app.paramsKeeper.Subspace(crisis.DefaultParamspace)
	app.subspaces[evidence.ModuleName] = app.paramsKeeper.Subspace(evidence.DefaultParamspace)
	app.subspaces[gov.ModuleName] = app.paramsKeeper.Subspace(gov.DefaultParamspace).WithKeyTable(gov.ParamKeyTable())
	app.subspaces[bandwidth.ModuleName] = app.paramsKeeper.Subspace(bandwidth.DefaultParamspace).WithKeyTable(bandwidth.ParamKeyTable())
	app.subspaces[rank.ModuleName] = app.paramsKeeper.Subspace(rank.DefaultParamspace).WithKeyTable(rank.ParamKeyTable())

	// add keepers
	app.accountKeeper = auth.NewAccountKeeper(
		app.cdc, dbKeys.acc, app.subspaces[auth.ModuleName], auth.ProtoBaseAccount,
	)
	bankKeeper := cyberbank.NewKeeper(
		app.accountKeeper, app.subspaces[sdkbank.ModuleName], app.ModuleAccountAddrs(),
	)
	app.bankKeeper = bankKeeper // TODO
	app.supplyKeeper = supply.NewKeeper(
		app.cdc, dbKeys.supply, app.accountKeeper, app.bankKeeper, maccPerms,
	)
	stakingKeeper := staking.NewKeeper(
		app.cdc, dbKeys.stake, app.supplyKeeper, app.subspaces[staking.ModuleName],
	)
	app.bankKeeper.SetStakingKeeper(&stakingKeeper) // TODO
	app.bankKeeper.SetSupplyKeeper(app.supplyKeeper) // TODO
	app.mintKeeper = mint.NewKeeper(
		app.cdc, dbKeys.mint, app.subspaces[mint.ModuleName], &stakingKeeper,
		app.supplyKeeper, auth.FeeCollectorName,
	)
	app.distrKeeper = distr.NewKeeper(
		app.cdc, dbKeys.distr, app.subspaces[distr.ModuleName], &stakingKeeper,
		app.supplyKeeper, auth.FeeCollectorName, app.ModuleAccountAddrs(),
	)
	app.slashingKeeper = slashing.NewKeeper(
		app.cdc, dbKeys.slashing, &stakingKeeper, app.subspaces[slashing.ModuleName],
	)
	app.crisisKeeper = crisis.NewKeeper(
		app.subspaces[crisis.ModuleName], invCheckPeriod, app.supplyKeeper, auth.FeeCollectorName,
	)
	app.upgradeKeeper = upgrade.NewKeeper(skipUpgradeHeights, dbKeys.upgrade, app.cdc)
	evidenceKeeper := evidence.NewKeeper(
		app.cdc, dbKeys.evidence, app.subspaces[evidence.ModuleName], &stakingKeeper, app.slashingKeeper,
	)
	evidenceRouter := evidence.NewRouter()
	evidenceKeeper.SetRouter(evidenceRouter)
	app.evidenceKeeper = *evidenceKeeper


	app.accBandwidthKeeper = bandwidth.NewAccBandwidthKeeper(dbKeys.accBandwidth, app.subspaces[bandwidth.ModuleName])
	app.blockBandwidthKeeper = bandwidth.NewBlockSpentBandwidthKeeper(dbKeys.blockBandwidth)

	// register the proposal types
	govRouter := gov.NewRouter().
		AddRoute(gov.RouterKey, gov.ProposalHandler).
		AddRoute(params.RouterKey, params.NewParamChangeProposalHandler(app.paramsKeeper)).
		AddRoute(distr.RouterKey, distr.NewCommunityPoolSpendProposalHandler(app.distrKeeper)).
		AddRoute(upgrade.RouterKey, upgrade.NewSoftwareUpgradeProposalHandler(app.upgradeKeeper))

	app.govKeeper = gov.NewKeeper(
		app.cdc, dbKeys.gov, app.subspaces[gov.ModuleName],
		app.supplyKeeper, &stakingKeeper, govRouter,
	)

	// cyberd keepers
	app.linkIndexedKeeper = link.NewIndexedKeeper(link.NewLinkKeeper(mainKeeper, dbKeys.links))
	app.cidNumKeeper = link.NewCidNumberKeeper(mainKeeper, dbKeys.cidNum, dbKeys.cidNumReverse)
	app.stakingIndexKeeper = cyberbank.NewIndexedKeeper(bankKeeper)
	app.rankStateKeeper = rank.NewStateKeeper(app.cdc, app.subspaces[rank.ModuleName],
		allowSearch, app.mainKeeper, app.stakingIndexKeeper,
		app.linkIndexedKeeper, app.cidNumKeeper, app.paramsKeeper, computeUnit,
	)

	app.stakingKeeper = *stakingKeeper.SetHooks(
		staking.NewMultiStakingHooks(app.distrKeeper.Hooks(), app.slashingKeeper.Hooks()),
	)

	app.bankKeeper.AddHook(bandwidth.CollectAddressesWithStakeChange())

	app.bandwidthMeter = bandwidth.NewBaseMeter(
		app.mainKeeper, app.paramsKeeper, app.accountKeeper, app.accBandwidthKeeper,
		app.blockBandwidthKeeper, app.bankKeeper, bandwidth.MsgBandwidthCosts,
	)

	// NOTE: Any module instantiated in the module manager that is later modified
	// must be passed by reference here.
	app.mm = module.NewManager(
		genutil.NewAppModule(app.accountKeeper, app.stakingKeeper, app.BaseApp.DeliverTx), // TODO
		auth.NewAppModule(app.accountKeeper),
		sdkbank.NewAppModule(app.bankKeeper, app.accountKeeper),
		crisis.NewAppModule(&app.crisisKeeper),
		supply.NewAppModule(app.supplyKeeper, app.accountKeeper),
		gov.NewAppModule(app.govKeeper, app.accountKeeper, app.supplyKeeper),
		mint.NewAppModule(app.mintKeeper),
		slashing.NewAppModule(app.slashingKeeper, app.accountKeeper, app.stakingKeeper),
		distr.NewAppModule(app.distrKeeper, app.accountKeeper, app.supplyKeeper, app.stakingKeeper),
		staking.NewAppModule(app.stakingKeeper, app.accountKeeper, app.supplyKeeper),
		upgrade.NewAppModule(app.upgradeKeeper),
		evidence.NewAppModule(app.evidenceKeeper),

		bandwidth.NewAppModule(app.accBandwidthKeeper, app.blockBandwidthKeeper),
		link.NewAppModule(app.cidNumKeeper, app.linkIndexedKeeper, app.accountKeeper),
		rank.NewAppModule(app.rankStateKeeper),
	)

	app.mm.RegisterInvariants(&app.crisisKeeper)
	app.mm.RegisterRoutes(app.Router(), app.QueryRouter())

	// TODO
	app.MountStores(
		dbKeys.main, dbKeys.acc, dbKeys.cidNum, dbKeys.cidNumReverse, dbKeys.links, dbKeys.rank, dbKeys.stake,
		dbKeys.slashing, dbKeys.gov, dbKeys.params, dbKeys.distr, dbKeys.accBandwidth,
		dbKeys.blockBandwidth, dbKeys.tParams, dbKeys.tStake, dbKeys.mint, dbKeys.supply, dbKeys.upgrade, dbKeys.evidence,
	)

	app.SetInitChainer(app.applyGenesis)
	app.SetBeginBlocker(app.BeginBlocker)
	app.SetEndBlocker(app.EndBlocker)
	//because genesis max_gas equals -1 there is NewInfiniteGasMeter
	app.SetAnteHandler(auth.NewAnteHandler(app.accountKeeper, app.supplyKeeper, auth.DefaultSigVerificationGasConsumer))

	if loadLatest {
		err := app.LoadLatestVersion(dbKeys.main)
		if err != nil {
			tmos.Exit(err.Error())
		}
	}

	ctx := app.BaseApp.NewContext(true, abci.Header{})
	app.latestBlockHeight = int64(mainKeeper.GetLatestBlockNumber(ctx))
	ctx = ctx.WithBlockHeight(app.latestBlockHeight)

	bandwidthParamset := bandwidth.NewDefaultParams()
	app.accBandwidthKeeper.SetParams(ctx, bandwidthParamset)

	rankParamset := rank.NewDefaultParams()
	app.rankStateKeeper.SetParams(ctx, rankParamset)

	// build context for current rank calculation round
	calculationPeriod := app.rankStateKeeper.GetParams(ctx).CalculationPeriod

	rankRoundBlockNumber := (app.latestBlockHeight / calculationPeriod) * calculationPeriod
	if rankRoundBlockNumber == 0 && app.latestBlockHeight >= 1 {
		rankRoundBlockNumber = 1 // special case cause tendermint blocks start from 1
	}
	rankCtx, err := util.NewContextWithMSVersion(db, rankRoundBlockNumber, dbKeys.GetStoreKeys()...)
	if err != nil {
		tmos.Exit(err.Error())
	}

	// IN-MEMORY DATA
	start := time.Now()
	app.BaseApp.Logger().Info("Loading mem state")
	app.linkIndexedKeeper.Load(rankCtx, ctx)
	app.stakingIndexKeeper.Load(rankCtx, ctx)
	app.BaseApp.Logger().Info("App loaded", "time", time.Since(start))

	// BANDWIDTH LOAD
	app.bandwidthMeter.Load(ctx)

	// RANK PARAMS
	app.rankStateKeeper.Load(ctx, app.Logger())

	app.Seal()
	return app
}

func (app *CyberdApp) applyGenesis(ctx sdk.Context, req abci.RequestInitChain) abci.ResponseInitChain {
	start := time.Now()
	app.Logger().Info("Applying genesis")

	var genesisState GenesisState
	err := app.cdc.UnmarshalJSON(req.AppStateBytes, &genesisState)
	if err != nil {
		panic(err)
	}

	// load the accounts
	for _, account := range genesisState.Accounts {
		app.accountKeeper.GetNextAccountNumber(ctx)
		app.accountKeeper.SetAccount(ctx, account)
		app.stakingIndexKeeper.UpdateStake(
			types.AccNumber(account.GetAccountNumber()),
			account.GetCoins().AmountOf(coin.CYB).Int64())
	}

	cyberbank.InitGenesis(ctx, app.bankKeeper, genesisState.BankData)
	// initialize distribution (must happen before staking)
	distr.InitGenesis(ctx, app.distrKeeper, app.supplyKeeper, genesisState.DistrData)
	supply.InitGenesis(ctx, app.supplyKeeper, app.accountKeeper, genesisState.SupplyData)

	validators := staking.InitGenesis(
		ctx, app.stakingKeeper, app.accountKeeper, app.supplyKeeper, genesisState.StakingData,
	)
	auth.InitGenesis(ctx, app.accountKeeper, genesisState.AuthData)
	slashing.InitGenesis(ctx, app.slashingKeeper, app.stakingKeeper, genesisState.SlashingData)
	gov.InitGenesis(ctx, app.govKeeper, app.supplyKeeper, genesisState.GovData)
	mint.InitGenesis(ctx, app.mintKeeper, genesisState.MintData)
	bandwidth.InitGenesis(ctx, app.bandwidthMeter, app.accBandwidthKeeper, genesisState.GetAddresses(),
		genesisState.BandwidthData)
	rank.InitGenesis(ctx, app.rankStateKeeper, genesisState.RankData)

	err = link.InitGenesis(ctx, app.cidNumKeeper, app.linkIndexedKeeper, app.Logger())
	if err != nil {
		panic(err)
	}

	crisis.InitGenesis(ctx, app.crisisKeeper, genesisState.Crisis)
	evidence.InitGenesis(ctx, app.evidenceKeeper, evidence.DefaultGenesisState())

	err = validateGenesisState(genesisState)
	if err != nil {
		panic(err)
	}

	if len(genesisState.GenTxs.GenTxs) > 0 { // TODO
		for _, genTx := range genesisState.GenTxs.GenTxs {
			var tx auth.StdTx
			err = app.cdc.UnmarshalJSON(genTx, &tx)
			if err != nil {
				panic(err)
			}
			bz := abci.RequestDeliverTx{Tx: app.cdc.MustMarshalBinaryLengthPrefixed(tx)}
			res := app.BaseApp.DeliverTx(bz)
			if !res.IsOK() {
				panic(res.Log)
			}
		}

		validators = app.stakingKeeper.ApplyAndReturnValidatorSetUpdates(ctx)
	}

	if len(req.Validators) > 0 {
		if len(req.Validators) != len(validators) {
			panic(fmt.Errorf("len(RequestInitChain.Validators) != len(validators) (%d != %d)",
				len(req.Validators), len(validators)))
		}
		sort.Sort(abci.ValidatorUpdates(req.Validators))
		sort.Sort(abci.ValidatorUpdates(validators))
		for i, val := range validators {
			if !val.Equal(req.Validators[i]) {
				panic(fmt.Errorf("validators[%d] != req.Validators[%d] ", i, i))
			}
		}
	}

	app.Logger().Info("Genesis applied", "time", time.Since(start))
	return abci.ResponseInitChain{
		Validators: validators,
	}
}

func (app *CyberdApp) CheckTx(req abci.RequestCheckTx) (res abci.ResponseCheckTx) {

	ctx := app.NewContext(true, abci.Header{Height: app.latestBlockHeight})
	tx, acc, err := app.decodeTxAndAccount(ctx, req.GetTx())

	if err != nil {
		return sdkerrors.ResponseCheckTx(err, 0, 0)
	}

	if err == nil {

		txCost := app.bandwidthMeter.GetPricedTxCost(ctx, tx)
		accBw := app.bandwidthMeter.GetCurrentAccBandwidth(ctx, acc)

		curBlockSpentBandwidth := app.bandwidthMeter.GetCurBlockSpentBandwidth(ctx)
		maxBlockBandwidth := app.bandwidthMeter.GetMaxBlockBandwidth(ctx)

		if !accBw.HasEnoughRemained(txCost) {
			err = types.ErrNotEnoughBandwidth
		} else if (uint64(txCost) + curBlockSpentBandwidth) > maxBlockBandwidth  {
			err = types.ErrExceededMaxBlockBandwidth
		} else {
			resp := app.BaseApp.CheckTx(req)
			if resp.Code == 0 {
				app.bandwidthMeter.ConsumeAccBandwidth(ctx, accBw, txCost)
			}
			return resp
		}
	}

	return sdkerrors.ResponseCheckTx(err, 0, 0)
}

func (app *CyberdApp) DeliverTx(req abci.RequestDeliverTx) (res abci.ResponseDeliverTx) {

	ctx := app.NewContext(false, abci.Header{Height: app.latestBlockHeight})
	tx, acc, err := app.decodeTxAndAccount(ctx, req.GetTx())

	if err != nil {
		return sdkerrors.ResponseDeliverTx(err, 0, 0)
	}

	if err == nil {

		txCost := app.bandwidthMeter.GetPricedTxCost(ctx, tx)
		accBw := app.bandwidthMeter.GetCurrentAccBandwidth(ctx, acc)

		curBlockSpentBandwidth := app.bandwidthMeter.GetCurBlockSpentBandwidth(ctx)
		maxBlockBandwidth := app.bandwidthMeter.GetMaxBlockBandwidth(ctx)

		if !accBw.HasEnoughRemained(txCost) {
			err = types.ErrNotEnoughBandwidth
		} else if (uint64(txCost) + curBlockSpentBandwidth) > maxBlockBandwidth  {
			err = types.ErrExceededMaxBlockBandwidth
		} else {
			resp := app.BaseApp.DeliverTx(req)
			app.bandwidthMeter.ConsumeAccBandwidth(ctx, accBw, txCost)

			linkingCost := app.bandwidthMeter.GetPricedLinksCost(ctx, tx)
			if linkingCost != int64(0) {
				app.bandwidthMeter.UpdateLinkedBandwidth(ctx, accBw, linkingCost)
			}

			app.bandwidthMeter.AddToBlockBandwidth(app.bandwidthMeter.GetTxCost(ctx, tx))

			return resp
		}
	}

	return sdkerrors.ResponseDeliverTx(err, 0, 0)
}

func (app *CyberdApp) decodeTxAndAccount(ctx sdk.Context, txBytes []byte) (auth.StdTx, sdk.AccAddress, error) {

	decoded, err := app.txDecoder(txBytes)
	if err != nil {
		return auth.StdTx{}, nil, err
	}

	tx := decoded.(auth.StdTx)
	if tx.GetMsgs() == nil || len(tx.GetMsgs()) == 0 {
		return tx, nil, sdkerrors.ErrInvalidRequest
	}

	if err := tx.ValidateBasic(); err != nil {
		return tx, nil, err
	}

	// signers acc [0] bandwidth will be consumed
	account := tx.GetSigners()[0]
	acc := app.accountKeeper.GetAccount(ctx, account)
	if acc == nil {
		return tx, nil, sdkerrors.ErrUnknownAddress
	}

	return tx, account, nil
}

func (app *CyberdApp) BeginBlocker(ctx sdk.Context, req abci.RequestBeginBlock) abci.ResponseBeginBlock {

	// mint new tokens for the previous block
	mint.BeginBlocker(ctx, app.mintKeeper)
	// distribute rewards for the previous block
	distr.BeginBlocker(ctx, req, app.distrKeeper)

	// slash anyone who double signed.
	// NOTE: This should happen after distr.BeginBlocker so that
	// there is nothing left over in the validator fee pool,
	// so as to keep the CanWithdrawInvariant invariant.
	slashing.BeginBlocker(ctx, req, app.slashingKeeper)
	return abci.ResponseBeginBlock{Events: ctx.EventManager().ABCIEvents()}
}

// Calculates cyber.Rank for block N, and returns Hash of result as app state.
// Calculated app state will be included in N+1 block header, thus influence on block hash.
// App state is consensus driven state.
func (app *CyberdApp) EndBlocker(ctx sdk.Context, _ abci.RequestEndBlock) abci.ResponseEndBlock {

	app.latestBlockHeight = ctx.BlockHeight()
	app.mainKeeper.StoreLatestBlockNumber(ctx, uint64(ctx.BlockHeight()))
	crisis.EndBlocker(ctx, app.crisisKeeper)
	gov.EndBlocker(ctx, app.govKeeper)
	validatorUpdates := staking.EndBlocker(ctx, app.stakingKeeper)
	app.stakingIndexKeeper.EndBlocker(ctx)

	bandwidth.EndBlocker(ctx, app.paramsKeeper, app.bandwidthMeter)

	// RANK CALCULATION
	app.rankStateKeeper.EndBlocker(ctx, app.Logger())

	return abci.ResponseEndBlock{
		ValidatorUpdates: validatorUpdates,
		Events:           ctx.EventManager().ABCIEvents(),
	}
}

// Implements ABCI
func (app *CyberdApp) Commit() (res abci.ResponseCommit) {
	err := app.linkIndexedKeeper.Commit(uint64(app.latestBlockHeight))
	if err != nil {
		panic(err)
	}
	app.BaseApp.Commit()
	return abci.ResponseCommit{Data: app.appHash()}
}

// Implements ABCI
func (app *CyberdApp) Info(req abci.RequestInfo) abci.ResponseInfo {

	return abci.ResponseInfo{
		Data:             app.BaseApp.Name(),
		LastBlockHeight:  app.LastBlockHeight(),
		LastBlockAppHash: app.appHash(),
	}
}

func (app *CyberdApp) appHash() []byte {

	if app.LastBlockHeight() == 0 {
		return make([]byte, 0)
	}

	linkHash := app.linkIndexedKeeper.GetNetworkLinkHash()
	rankHash := app.rankStateKeeper.GetNetworkRankHash()

	result := make([]byte, len(linkHash))

	for i := 0; i < len(linkHash); i++ {
		result[i] = linkHash[i] ^ rankHash[i]
	}

	return result
}

func (app *CyberdApp) LoadHeight(height int64) error {
	return app.LoadVersion(height, app.dbKeys.main)
}

// ModuleAccountAddrs returns all the app's module account addresses.
func (app *CyberdApp) ModuleAccountAddrs() map[string]bool {
	modAccAddrs := make(map[string]bool)
	for acc := range maccPerms {
		modAccAddrs[supply.NewModuleAddress(acc).String()] = true
	}

	return modAccAddrs
}

// GetMaccPerms returns a mapping of the application's module account permissions.
func GetMaccPerms() map[string][]string {
	modAccPerms := make(map[string][]string)
	for k, v := range maccPerms {
		modAccPerms[k] = v
	}
	return modAccPerms
}