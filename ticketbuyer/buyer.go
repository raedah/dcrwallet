// Copyright (c) 2016 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package ticketbuyer

import (
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/decred/dcrd/blockchain"
	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrrpcclient"
	"github.com/decred/dcrutil"
	"github.com/decred/dcrwallet/waddrmgr"
	"github.com/decred/dcrwallet/wallet"
)

var (
	// zeroUint32 is the zero value for a uint32.
	zeroUint32 = uint32(0)

	// stakeInfoReqTries is the maximum number of times to try
	// GetStakeInfo before failing.
	stakeInfoReqTries = 20

	// stakeInfoReqTryDelay is the time in seconds to wait before
	// doing another GetStakeInfo request.
	stakeInfoReqTryDelay = time.Second * 1
)

const (
	// TicketFeeMean is the string indicating that the mean ticket fee
	// should be used when determining ticket fee.
	TicketFeeMean = "mean"

	// TicketFeeMedian is the string indicating that the median ticket fee
	// should be used when determining ticket fee.
	TicketFeeMedian = "median"

	// PriceTargetVWAP is the string indicating that the volume
	// weighted average price should be used as the price target.
	PriceTargetVWAP = "vwap"

	// PriceTargetPool is the string indicating that the ticket pool
	// price should be used as the price target.
	PriceTargetPool = "pool"

	// PriceTargetDual is the string indicating that a combination of the
	// ticket pool price and the ticket VWAP should be used as the
	// price target.
	PriceTargetDual = "dual"
)

// Config stores the configuration options for ticket buyer.
type Config struct {
	AccountName               string
	AvgPriceMode              string
	AvgPriceVWAPDelta         int
	BalanceToMaintainAbsolute float64
	BalanceToMaintainRelative float64
	BlocksToAvg               int
	DontWaitForTickets        bool
	ExpiryDelta               int
	FeeSource                 string
	FeeTargetScaling          float64
	HighPricePenalty          float64
	MinFee                    float64
	MaxFee                    float64
	MaxPerBlock               int
	MaxPriceAbsolute          float64
	MaxPriceRelative          float64
	MaxPriceScale             float64
	MaxInMempool              int
	PoolAddress               string
	PoolFees                  float64
	PriceTarget               float64
	SpreadTicketPurchases     bool
	TicketAddress             string
	TxFee                     float64
	PrevToBuyDiffPeriod       int
	PrevToBuyHeight           int
}

// TicketPurchaser is the main handler for purchasing tickets. It decides
// whether or not to do so based on information obtained from daemon and
// wallet chain servers.
type TicketPurchaser struct {
	cfg           *Config
	activeNet     *chaincfg.Params
	dcrdChainSvr  *dcrrpcclient.Client
	wallet        *wallet.Wallet
	ticketAddress dcrutil.Address
	poolAddress   dcrutil.Address
	firstStart    bool
	windowPeriod  int          // The current window period
	idxDiffPeriod int          // Relative block index within the difficulty period
	useMedian     bool         // Flag for using median for ticket fees
	priceMode     avgPriceMode // Price mode to use to calc average price
	heightCheck   map[int64]struct{}
	balEstimated  dcrutil.Amount
	//ticketPrice      dcrutil.Amount
	stakePoolSize    uint32
	stakeLive        uint32
	stakeImmature    uint32
	stakeVoteSubsidy dcrutil.Amount
}

// NewTicketPurchaser creates a new TicketPurchaser.
func NewTicketPurchaser(cfg *Config,
	dcrdChainSvr *dcrrpcclient.Client,
	w *wallet.Wallet,
	activeNet *chaincfg.Params) (*TicketPurchaser, error) {
	var ticketAddress dcrutil.Address
	var err error
	if cfg.TicketAddress != "" {
		ticketAddress, err = dcrutil.DecodeAddress(cfg.TicketAddress,
			activeNet)
		if err != nil {
			return nil, err
		}
	}
	var poolAddress dcrutil.Address
	if cfg.PoolAddress != "" {
		poolAddress, err = dcrutil.DecodeNetworkAddress(cfg.PoolAddress)
		if err != nil {
			return nil, err
		}
	}

	priceMode := avgPriceMode(AvgPriceVWAPMode)
	switch cfg.AvgPriceMode {
	case PriceTargetPool:
		priceMode = AvgPricePoolMode
	case PriceTargetDual:
		priceMode = AvgPriceDualMode
	}

	return &TicketPurchaser{
		cfg:           cfg,
		activeNet:     activeNet,
		dcrdChainSvr:  dcrdChainSvr,
		wallet:        w,
		firstStart:    true,
		ticketAddress: ticketAddress,
		poolAddress:   poolAddress,
		useMedian:     cfg.FeeSource == TicketFeeMedian,
		priceMode:     priceMode,
		heightCheck:   make(map[int64]struct{}),
	}, nil
}

// PurchaseStats stats is a collection of statistics related to the ticket purchase.
type PurchaseStats struct {
	Height     int64
	Purchased  int
	LeftWindow int
}

// Purchase is the main handler for purchasing tickets for the user.
// TODO Not make this an inlined pile of crap.
func (t *TicketPurchaser) Purchase(height int64) (*PurchaseStats, error) {

	ps := &PurchaseStats{Height: height}

	//
	// Startup checks

	if t.wallet.Locked() {
		return ps, fmt.Errorf("Wallet not unlocked to allow ticket purchases")
	}
	avgPriceAmt, err := t.calcAverageTicketPrice(height)
	if err != nil {
		return ps, fmt.Errorf("Failed to calculate average ticket price amount: %s",
			err.Error())
	}

	// Check to make sure that the current height has not already been seen for a reorg or a fork
	if _, exists := t.heightCheck[height]; exists {
		log.Debugf("We've already seen this height, reorg/fork detected at height %v", height)
		return ps, nil
	}
	t.heightCheck[height] = struct{}{}

	// Initialize based on where we are in the window
	winSize := t.activeNet.StakeDiffWindowSize
	maxStake := int(t.activeNet.MaxFreshStakePerBlock)

	refreshStakeInfo := false
	if t.firstStart {
		t.firstStart = false
		log.Debugf("First run for ticket buyer")
		log.Debugf("Transaction relay fee: %v DCR", t.cfg.TxFee)
		refreshStakeInfo = true
	}
	if int((height+3)%winSize) == 0 {
		log.Debugf("***Last time to buy is now")
	}
	if int((height+2)%winSize) == 0 {
		// Starting a new window
		log.Debugf("**No more buying")
		log.Debugf("**Your last pre-tx generations were mined and your last sstxs will mine in the next block")
	}
	if int((height+1)%winSize) == 0 {
		// can not buy last bloc, ref dcrticketbuyer/issues/66
		log.Debugf("**The last stake window is complete. The next block is a new stake difficulty")
		log.Debugf("**Resetting stake window variables")
		refreshStakeInfo = true
	}
	if int(height%winSize) != 0 && int(height/winSize) > t.windowPeriod {
		// Disconnected and reconnected in a different window
		log.Debugf("**Reconnected in a different window, now at height %v", height)
		refreshStakeInfo = true
	}

	//
	// Load needed information
	//

	t.idxDiffPeriod = int(height % winSize)
	t.windowPeriod = int(height / winSize)
	account, err := t.wallet.AccountNumber(t.cfg.AccountName)
	if err != nil {
		return ps, err
	}
	bal, err := t.wallet.CalculateAccountBalance(account, 0)
	if err != nil {
		return ps, err
	}
	memPoolOwn, err := t.ownTicketsInMempool()
	if err != nil {
		return ps, err
	}
	memPoolAll, err := t.allTicketsInMempool()
	if err != nil {
		return ps, err
	}
	estStakeDiff, err := t.dcrdChainSvr.EstimateStakeDiff(nil)
	if err != nil {
		return ps, err
	}
	stakeDiff, err := t.wallet.StakeDifficulty()
	if err != nil {
		return ps, err
	}
	oneBlock := uint32(1)
	ticketFeeInfo, err := t.dcrdChainSvr.TicketFeeInfo(&oneBlock, &zeroUint32)
	if err != nil {
		return ps, err
	}
	if len(ticketFeeInfo.FeeInfoBlocks) < 1 {
		return ps, fmt.Errorf("feeinfo blocks bad length")
	}
	ticketPurchasesInLastBlock := int(ticketFeeInfo.FeeInfoBlocks[0].Number)

	blocksRemaining := int(winSize) - t.idxDiffPeriod - 2
	if blocksRemaining < 1 {
		log.Infof("No blocks remaining to buy")
		return ps, nil
	}

	//
	// Decide what values to use
	//

	// cache stakeinfo
	if refreshStakeInfo {
		log.Debugf("Getting StakeInfo")
		var curStakeInfo *wallet.StakeInfoData
		var err error
		for i := 1; i <= stakeInfoReqTries; i++ {
			curStakeInfo, err = t.wallet.StakeInfo()
			if err != nil {
				log.Debugf("Waiting for StakeInfo, attempt %v: (%v)", i, err.Error())
				time.Sleep(stakeInfoReqTryDelay)
				continue
			}
			if err == nil {
				log.Debugf("Got StakeInfo")
				break
			}
		}

		if err != nil {
			return ps, err
		}
		t.stakePoolSize = curStakeInfo.PoolSize
		t.stakeLive = curStakeInfo.Live
		t.stakeImmature = curStakeInfo.Immature

		subsidyCache := blockchain.NewSubsidyCache(height, t.wallet.ChainParams())
		subsidy := blockchain.CalcStakeVoteSubsidy(subsidyCache, height, t.wallet.ChainParams())
		t.stakeVoteSubsidy = dcrutil.Amount(subsidy)
		log.Debugf("Stake vote subsidy: %v", t.stakeVoteSubsidy)
		proportionLive := float64(t.stakeLive) / float64(t.stakePoolSize)
		log.Debugf("Proportion live: %.8f%%", proportionLive)
		// 24 hours * 60 minutes in a day
		generatedPerDay := t.stakeVoteSubsidy.ToCoin() * float64(t.activeNet.TicketsPerBlock) *
			(24 * 60 / float64(t.activeNet.TargetTimePerBlock.Minutes())) * proportionLive
		log.Debugf("Stake revenue per day: ~%.8f DCR (minus fees)", generatedPerDay)
	}

	// find and set scaled ticket fee
	var feeToUse float64
	if ticketPurchasesInLastBlock < maxStake && memPoolAll < maxStake {
		log.Debugf("Using min ticket fee: %.8f DCR", t.cfg.MinFee)
		feeToUse = t.cfg.MinFee
	} else {
		// if not enough recent blocks to average fees, use data from the last
		// window with the closest difficulty
		chainFee := 0.0
		if t.idxDiffPeriod < t.cfg.BlocksToAvg {
			chainFee, err = t.findClosestFeeWindows(stakeDiff.ToCoin(),
				t.useMedian)
			if err != nil {
				return ps, err
			}
		} else {
			chainFee, err = t.findTicketFeeBlocks(t.useMedian)
			if err != nil {
				return ps, err
			}
		}
		// Scale the mean fee upwards according to what was asked for by the user.
		feeToUse = chainFee * t.cfg.FeeTargetScaling
		log.Tracef("Average ticket fee: %.8f DCR", chainFee)
		if feeToUse > t.cfg.MaxFee {
			log.Infof("Not buying because max fee exceed: (max fee: %.8f DCR,  scaled fee: %.8f DCR)",
				t.cfg.MaxFee, feeToUse)
			return ps, nil
		}
		if feeToUse < t.cfg.MinFee {
			log.Debugf("Using min ticket fee: %.8f DCR (scaled fee: %.8f DCR)", t.cfg.MinFee, feeToUse)
			feeToUse = t.cfg.MinFee
		} else {
			log.Tracef("Using scaled ticket fee: %.8f DCR", feeToUse)
		}
	}
	feeToUseAmt, err := dcrutil.NewAmount(feeToUse)
	if err != nil {
		return ps, err
	}
	t.wallet.SetTicketFeeIncrement(feeToUseAmt)

	// Set the balancetomaintain to the configuration parameter that is higher
	// Absolute or relative balance to maintain
	var balanceToMaintainAmt dcrutil.Amount
	if t.cfg.BalanceToMaintainAbsolute > 0 && t.cfg.BalanceToMaintainAbsolute >
		bal.Total.ToCoin()*t.cfg.BalanceToMaintainRelative {

		balanceToMaintainAmt, err = dcrutil.NewAmount(t.cfg.BalanceToMaintainAbsolute)
		if err != nil {
			return ps, err
		}
		log.Debugf("Using absolute balancetomaintain: %v", balanceToMaintainAmt)
	} else {
		balanceToMaintainAmt, err = dcrutil.NewAmount(bal.Total.ToCoin() * t.cfg.BalanceToMaintainRelative)
		if err != nil {
			return ps, err
		}
		log.Debugf("Using relative balancetomaintain: %v", balanceToMaintainAmt)
	}

	// maxperblock. positive numbers mean that many tickets per block.
	// negative numbers mean to only purchase one ticket once every abs(num) blocks.
	maxPerBlock := 0
	switch {
	case t.cfg.MaxPerBlock == 0:
		return ps, nil
	case t.cfg.MaxPerBlock > 0:
		maxPerBlock = t.cfg.MaxPerBlock
	case t.cfg.MaxPerBlock < 0:
		if int(height)%t.cfg.MaxPerBlock != 0 {
			return ps, nil
		}
		maxPerBlock = 1
	}

	// config
	BASE_RESERVE := 2.0         // amount of funds to keep in reserve measured by spendPerWindows
	TARGET_SUPPORT := 0.2       // keep the price from dropping too rapidly, aim this much between avg and high
	AUTO_TARGET_PRICE := 2000.0 // tunes the target price relative to the block height
	MAX_PRICE_MULTIPLIER := 5.0 // scale up the max price when your reserve funds are growing

	avgWinPeriods := float64(t.activeNet.TicketPoolSize) / float64(winSize) // win periods per investment maturity
	windowRatio := float64(t.idxDiffPeriod) / float64(winSize)              // how far into the window we are
	// calculate dynamic price target scaling
	// note, winperiods for mainnet: 8192 / 144 =  ~56.88  (~28.44 days)
	spendPerWindow := bal.Total.ToCoin() / avgWinPeriods  // how much we should be spending per window
	fundsRatio := bal.Spendable.ToCoin() / spendPerWindow // how much we have over how much we should have at this point

	//targetPrice := avgPriceAmt.ToCoin()
	//targetPrice = stakeDiff.ToCoin()
	targetPrice := float64(height) / AUTO_TARGET_PRICE
	if t.cfg.PriceTarget > 0.0 {
		targetPrice = t.cfg.PriceTarget
	}
	log.Debugf("Using target price: %v", targetPrice)
	maxPriceScale := ((1 / avgWinPeriods) * (fundsRatio - BASE_RESERVE)) + 1
	targetPrice = targetPrice * maxPriceScale
	log.Tracef("Dynamic price target: (Scale: %.0f%%, Amount: %.3f DCR)", maxPriceScale*100, targetPrice)

	// make sure the price does not drop too rapidly
	if targetPrice < stakeDiff.ToCoin() {
		targetPrice = ((stakeDiff.ToCoin() - targetPrice) * TARGET_SUPPORT) + targetPrice
		log.Debugf("Holding up target price: %v", targetPrice)
	}

	var maxPriceAmt dcrutil.Amount
	/*
		if t.cfg.MaxPriceAbsolute > 0 && t.cfg.MaxPriceAbsolute < targetPrice*t.cfg.MaxPriceRelative {
			maxPriceAmt, err = dcrutil.NewAmount(t.cfg.MaxPriceAbsolute)
			if err != nil {
				return ps, err
			}
			log.Debugf("Using absolute max price: %v", maxPriceAmt)
		} else {
			maxPriceAmt, err = dcrutil.NewAmount(targetPrice * t.cfg.MaxPriceRelative)
			if err != nil {
				return ps, err
			}
			log.Debugf("Using relative max price: %v", maxPriceAmt)
		}
	*/

	// Max price multiplied
	maxPriceAmt, err = dcrutil.NewAmount(targetPrice + (fundsRatio * MAX_PRICE_MULTIPLIER))
	if err != nil {
		return ps, err
	}
	log.Debugf("Using max price multiplied: %v", maxPriceAmt)

	toBuyForBlock := int(math.Floor((bal.Spendable.ToCoin() - balanceToMaintainAmt.ToCoin()) / stakeDiff.ToCoin()))
	if toBuyForBlock < 0 {
		toBuyForBlock = 0
	}
	proportionLive := float64(t.stakeLive) / float64(t.stakePoolSize)
	tixWillRedeem := float64(blocksRemaining) * float64(t.activeNet.TicketsPerBlock) * proportionLive
	yourAvgTixPrice := 0.0
	if t.stakeLive+t.stakeImmature != 0 {
		yourAvgTixPrice = bal.LockedByTickets.ToCoin() / float64(t.stakeLive+t.stakeImmature)
	}
	redeemedFunds := tixWillRedeem * yourAvgTixPrice
	stakeRewardFunds := tixWillRedeem * t.stakeVoteSubsidy.ToCoin()
	tixToBuyWithRedeemedFunds := redeemedFunds / stakeDiff.ToCoin()
	tixToBuyWithStakeRewardFunds := stakeRewardFunds / stakeDiff.ToCoin()
	tixCanBuy := (bal.Spendable.ToCoin() - balanceToMaintainAmt.ToCoin()) / stakeDiff.ToCoin()
	if tixCanBuy < 0 {
		tixCanBuy = 0
	}
	tixCanBuyAll := tixCanBuy + tixToBuyWithRedeemedFunds + tixToBuyWithStakeRewardFunds
	buyPerBlockAll := tixCanBuyAll / float64(blocksRemaining)
	ticketsLeftInWindow := (int(winSize) - t.idxDiffPeriod - 1) * maxStake
	purchaseSlotsLeftInWindow := blocksRemaining * maxStake
	proportionPossible := (float64(t.stakeLive) + float64(tixCanBuy)) / float64(t.stakePoolSize)

	canBuyRatio := tixCanBuy / float64(purchaseSlotsLeftInWindow)
	canBuyAllRatio := tixCanBuyAll / float64(purchaseSlotsLeftInWindow)
	stakeDiffCanReach := ((estStakeDiff.Max - estStakeDiff.Min) * canBuyRatio) + estStakeDiff.Min
	stakeDiffCanReachAll := ((estStakeDiff.Max - estStakeDiff.Min) * canBuyAllRatio) + estStakeDiff.Min

	//
	// Output what is known
	//

	log.Tracef("Activenet: (tixpoolsize: %v, winSize: %v, avgWinPeriods: %.2f)", t.activeNet.TicketPoolSize, winSize, avgWinPeriods)
	log.Tracef("--idxDiffPeriod %v (starts at 0), Window period %v, Window %.0f%% complete--", t.idxDiffPeriod, t.windowPeriod, windowRatio*100)
	log.Tracef("Balance for account '%s': (Available: %.2f DCR, Spendable: %.2f DCR, Total: %.2f DCR)",
		t.cfg.AccountName, bal.Spendable.ToCoin()-balanceToMaintainAmt.ToCoin(), bal.Spendable.ToCoin(), bal.Total.ToCoin())
	log.Tracef("Your spend per window: %.2f, Funds ratio: %.3f windows worth", spendPerWindow, fundsRatio)
	log.Tracef("Proportion Live: %.0f%%, Proportion Possible: %.0f%%", proportionLive*100, proportionPossible*100)
	log.Tracef("Tickets: (Last block: %v, Mempool all: %v, Mempool own: %v)", ticketPurchasesInLastBlock, memPoolAll, memPoolOwn)
	log.Tracef("Average ticket price (All: %.2f DCR, Yours: %.2f DCR)", avgPriceAmt.ToCoin(), yourAvgTixPrice)
	log.Debugf("Stake diff: (current: %.2f, min: %.2f, expected: %.2f, max: %.2f)",
		stakeDiff.ToCoin(), estStakeDiff.Min, estStakeDiff.Expected, estStakeDiff.Max)
	log.Debugf("Expected value: (Redeem %.2f DCR, buys %.1f tickets) (PoS Reward %.2f DCR, buys %.1f tickets)",
		redeemedFunds, tixToBuyWithRedeemedFunds, stakeRewardFunds, tixToBuyWithStakeRewardFunds)
	log.Debugf("Ticket slots left in window is %v (usable: %v), blocks left is %v (usable: %v)",
		ticketsLeftInWindow, purchaseSlotsLeftInWindow, (int(winSize) - t.idxDiffPeriod - 1), blocksRemaining)
	log.Infof("Can buy now (price target: %.2f DCR, total tix: %.1f, tix per block: %.1f)",
		stakeDiffCanReach, tixCanBuy, float64(maxStake)*canBuyRatio)
	log.Infof("Can buy all (price target: %.2f DCR, total tix: %.1f, tix per block: %.1f)",
		stakeDiffCanReachAll, tixCanBuyAll, float64(maxStake)*canBuyAllRatio)

	//
	// Main calculation block
	//

	if t.cfg.SpreadTicketPurchases && toBuyForBlock > 0 {
		if blocksRemaining > 0 && tixCanBuy > 0 {
			rand.Seed(time.Now().UTC().UnixNano())
			ticketRemainder := buyPerBlockAll - math.Floor(buyPerBlockAll)
			if rand.Float64() <= float64(ticketRemainder) {
				toBuyForBlock++
			}
		}
	}

	// Limit the amount of tickets you are buying per block so that you do not exceed maxpricescale
	if t.cfg.MaxPriceScale > 0.0 {
		// find the tickets needed to reach max price scale
		needRatio := (targetPrice - estStakeDiff.Min) / (estStakeDiff.Max - estStakeDiff.Min)
		needThisWindow := float64(purchaseSlotsLeftInWindow) * needRatio
		willTargetStakeDiff := ((estStakeDiff.Max - estStakeDiff.Min) * needRatio) + estStakeDiff.Min
		log.Infof("Want Target (price target: %.2f DCR: total tix: %.1f, tix per block: %.1f)",
			willTargetStakeDiff, needThisWindow, float64(maxStake)*needRatio)

		// This will be for targeting subsequent window prices to get the price to fall back
		// to our stable target range. Needs to connect to estimatestakediff algo for values
		/*
			multipliers := make([]float64, 4)
			multipliers[0] = 1.25
			multipliers[1] = 2.0
			multipliers[2] = 3.0
			multipliers[3] = 4.0

			var tmpneedRatio float64
			var tmpneedThisWindow float64
			var tmpwillTargetStakeDiff float64
			for _, multiplier := range multipliers {
				priceMult := targetPrice * multiplier
				if priceMult <= estStakeDiff.Min {
					log.Debugf("Undr min %.1f multiplier %.2f DCR", multiplier, priceMult)
				} else if priceMult > estStakeDiff.Min && priceMult < estStakeDiff.Max {
					tmpneedRatio = (priceMult - estStakeDiff.Min) / (estStakeDiff.Max - estStakeDiff.Min)
					tmpneedThisWindow = float64(purchaseSlotsLeftInWindow) * tmpneedRatio
					if tmpneedThisWindow <= tixCanBuyAll {
						needRatio = tmpneedRatio
						needThisWindow = tmpneedThisWindow
						targetPrice = priceMult
						willTargetStakeDiff = ((estStakeDiff.Max - estStakeDiff.Min) * needRatio) + estStakeDiff.Min
						log.Tracef("== Using %.2f multiplier, %.2f", multiplier, targetPrice)
						log.Infof("Want Target (price target: %.2f DCR: total tix: %.1f, tix per block: %.1f)",
							willTargetStakeDiff, needThisWindow, float64(maxStake)*needRatio)
						break
					} else {
						tmpwillTargetStakeDiff = ((estStakeDiff.Max - estStakeDiff.Min) * tmpneedRatio) + estStakeDiff.Min
						log.Debugf("Not Poss %.1f multiplier for %.2f (price target: %.2f DCR: total tix: %.1f, tix per block: %.1f)",
							multiplier, targetPrice*multiplier, tmpwillTargetStakeDiff, tmpneedThisWindow, float64(maxStake)*tmpneedRatio)
					}
				} else {
					log.Debugf("Over max %.1f multiplier %.2f DCR", multiplier, targetPrice*multiplier)
				}
			}
		*/

		// if what we were planning to buy is greater then what we need to stay within
		// our target price, then calc the new lower amount to buy per block and use that
		var ratio float64
		if canBuyRatio < needRatio {
			ratio = canBuyRatio
		} else {
			ratio = needRatio
		}

		toBuyForBlockFloat := float64(maxStake) * ratio
		if float64(toBuyForBlock) > toBuyForBlockFloat {
			rand.Seed(time.Now().UTC().UnixNano())
			toBuyForBlock = int(math.Floor(toBuyForBlockFloat))
			ticketRemainder := toBuyForBlockFloat - math.Floor(toBuyForBlockFloat)
			if rand.Float64() <= float64(ticketRemainder) {
				toBuyForBlock++
			}
		} else {
			// This is the case where the price target is so far out of reach
			// that we just dont even bother trying
			if stakeDiffCanReach/targetPrice < 1-proportionPossible {
				return ps, fmt.Errorf("Not buying because can not reach price target")
			}
		}
		// Log scale slope up for dynamic price target
		exponent := 1 / ratio
		if float64(blocksRemaining) < exponent {
			exponent = float64(blocksRemaining)
		}
		log.Tracef("trace, canbuyRatio %v", canBuyRatio)
		log.Tracef("trace, pre-ratio %+v", ratio)
		ratio = math.Pow(ratio, exponent)
		log.Tracef("trace, ratio %+v", ratio)
		log.Tracef("trace, exponent %v", exponent)
		toBuyForBlockFloat = float64(maxStake) * ratio
		log.Infof("Log scale buying, %.8f now and slope up (log ratio: %.8f)", toBuyForBlockFloat, ratio)
		rand.Seed(time.Now().UTC().UnixNano())
		toBuyForBlock = int(math.Floor(toBuyForBlockFloat))
		ticketRemainder := toBuyForBlockFloat - float64(toBuyForBlock)
		if rand.Float64() <= float64(ticketRemainder) {
			toBuyForBlock++
		}
	}

	//
	// Post checks
	//

	// only the maximum number of tickets at each block should be purchased, as specified by the user
	if toBuyForBlock > maxPerBlock {
		toBuyForBlock = maxPerBlock
		if maxPerBlock == 1 {
			log.Infof("Limiting to 1 purchase so that maxperblock is not exceeded")
		} else {
			log.Infof("Limiting to %d purchases so that maxperblock is not exceeded", maxPerBlock)
		}
	}
	if !t.cfg.DontWaitForTickets {
		if toBuyForBlock+memPoolOwn > t.cfg.MaxInMempool {
			toBuyForBlock = t.cfg.MaxInMempool - memPoolOwn
			log.Debugf("Limiting to %d purchases so that maxinmempool is not exceeded", toBuyForBlock)
		}
	}
	if stakeDiff > maxPriceAmt {
		log.Infof("Not buying because max price exceeded: "+
			"(max price: %v, ticket price: %v)", maxPriceAmt, stakeDiff)
		return ps, nil
	}
	if t.cfg.MaxPriceScale > 0.0 && (estStakeDiff.Expected > targetPrice) {
		log.Infof("Not buying because the next window estimate %v DCR is higher than "+
			"the scaled max price %v", estStakeDiff.Expected, targetPrice)
		return ps, nil
	}
	if !t.cfg.DontWaitForTickets {
		if memPoolOwn >= t.cfg.MaxInMempool {
			log.Infof("Currently waiting for %v tickets to enter the "+
				"blockchain before buying more tickets (in mempool: %v,"+
				" max allowed in mempool %v)", memPoolOwn-t.cfg.MaxInMempool,
				memPoolOwn, t.cfg.MaxInMempool)
			return ps, nil
		}
	}
	//fix this
	//also, structure to show all errors
	/*
		if toBuyForBlock == 0 {
			log.Infof("Not enough funds to buy tickets: (spendable: %v, balancetomaintain: %v) ",
				bal.Spendable.ToCoin(), balanceToMaintainAmt.ToCoin())
		}
	*/
	if toBuyForBlock <= 0 {
		log.Infof("Not buying any tickets this round")
		return ps, nil
	}

	// safety check
	notEnough := func(bal dcrutil.Amount, toBuy int, sd dcrutil.Amount) bool {
		return (bal.ToCoin() - float64(toBuy)*sd.ToCoin()) <
			balanceToMaintainAmt.ToCoin()
	}
	if notEnough(bal.Spendable, toBuyForBlock, stakeDiff) {
		for notEnough(bal.Spendable, toBuyForBlock, stakeDiff) {
			if toBuyForBlock == 0 {
				break
			}

			toBuyForBlock--
			log.Debugf("Not enough, decremented amount of tickets to buy")
		}

		if toBuyForBlock == 0 {
			log.Infof("Not buying because spendable balance would be %v "+
				"but balance to maintain is %v",
				(bal.Spendable.ToCoin() - float64(toBuyForBlock)*
					stakeDiff.ToCoin()),
				balanceToMaintainAmt)
			return ps, nil
		}
	}

	//
	// Purchase tickets
	//

	// If an address wasn't passed, create an internal address in the wallet for the ticket address
	var ticketAddress dcrutil.Address
	if t.ticketAddress != nil {
		ticketAddress = t.ticketAddress
	} else {
		ticketAddress, err =
			t.wallet.NewAddress(account, waddrmgr.InternalBranch)
		if err != nil {
			return ps, err
		}
	}
	poolFeesAmt, err := dcrutil.NewAmount(t.cfg.PoolFees)
	if err != nil {
		return ps, err
	}
	expiry := int32(int(height) + t.cfg.ExpiryDelta + 2)
	hashes, err := t.wallet.PurchaseTickets(0,
		maxPriceAmt,
		0,
		ticketAddress,
		account,
		toBuyForBlock,
		t.poolAddress,
		poolFeesAmt.ToCoin(),
		expiry,
		t.wallet.RelayFee(),
		t.wallet.TicketFeeIncrement(),
	)
	if err != nil {
		return ps, err
	}
	tickets, ok := hashes.([]*chainhash.Hash)
	if !ok {
		return nil, fmt.Errorf("Unable to decode ticket hashes")
	}
	ps.Purchased = toBuyForBlock
	for i := range tickets {
		log.Infof("Purchased ticket %v at stake difficulty %v (%v "+
			"fees per KB used)", tickets[i], stakeDiff.ToCoin(),
			feeToUseAmt.ToCoin())
	}
	bal, err = t.wallet.CalculateAccountBalance(account, 0)
	if err != nil {
		return ps, err
	}
	log.Debugf("Usable balance for account '%s' after purchases: %v", t.cfg.AccountName, bal.Spendable)
	return ps, nil
}
