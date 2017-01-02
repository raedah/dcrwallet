// Copyright (c) 2016 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package ticketbuyer

import (
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrrpcclient"
	"github.com/decred/dcrutil"
	"github.com/decred/dcrwallet/waddrmgr"
	"github.com/decred/dcrwallet/wallet"
	"github.com/decred/dcrwallet/wtxmgr"
)

var (
	// zeroUint32 is the zero value for a uint32.
	zeroUint32 = uint32(0)
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
	AccountName       string
	AvgPriceMode      string
	AvgPriceVWAPDelta int
	BalanceToMaintain float64
	BlocksToAvg       int
	BuyImmediately    bool
	FeeSource         string
	FeeTargetScaling  float64
	HighPricePenalty  float64
	MinFee            float64
	MinPriceScale     float64
	MaxFee            float64
	MaxPerBlock       int
	MaxPriceAbsolute  float64
	MaxPriceScale     float64
	PoolAddress       string
	PoolFees          float64
	PriceTarget       float64
	TicketAddress     string
	TxFee             float64
	TicketFeeInfo     bool
}

// TicketPurchaser is the main handler for purchasing tickets. It decides
// whether or not to do so based on information obtained from daemon and
// wallet chain servers.
type TicketPurchaser struct {
	cfg              *Config
	activeNet        *chaincfg.Params
	dcrdChainSvr     *dcrrpcclient.Client
	wallet           *wallet.Wallet
	ticketAddress    dcrutil.Address
	poolAddress      dcrutil.Address
	firstStart       bool
	windowPeriod     int          // The current window period
	idxDiffPeriod    int          // Relative block index within the difficulty period
	//maintainMaxPrice bool         // Flag for maximum price manipulation
	//maintainMinPrice bool         // Flag for minimum price manipulation
	useMedian        bool         // Flag for using median for ticket fees
	priceMode        avgPriceMode // Price mode to use to calc average price
	heightCheck      map[int64]struct{}
	ticketPrice      dcrutil.Amount
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

        /*
	maintainMaxPrice := false
	if cfg.MaxPriceScale > 0.0 {
		maintainMaxPrice = true
	}

	maintainMinPrice := false
	if cfg.MinPriceScale > 0.0 {
		maintainMinPrice = true
	}
        */

	priceMode := avgPriceMode(AvgPriceVWAPMode)
	switch cfg.AvgPriceMode {
	case PriceTargetPool:
		priceMode = AvgPricePoolMode
	case PriceTargetDual:
		priceMode = AvgPriceDualMode
	}

	return &TicketPurchaser{
		cfg:              cfg,
		activeNet:        activeNet,
		dcrdChainSvr:     dcrdChainSvr,
		wallet:           w,
		firstStart:       true,
		ticketAddress:    ticketAddress,
		poolAddress:      poolAddress,
		//maintainMaxPrice: maintainMaxPrice,
		//maintainMinPrice: maintainMinPrice,
		useMedian:        cfg.FeeSource == TicketFeeMedian,
		priceMode:        priceMode,
		heightCheck:      make(map[int64]struct{}),
	}, nil
}

// PurchaseStats stats is a collection of statistics related to the ticket purchase.
type PurchaseStats struct {
	Height        int64
	PriceMinScale float64
	PriceMaxScale float64
	PriceAverage  float64
	PriceCurrent  float64
	Purchased     int
	FeeMin        float64
	FeeMax        float64
	FeeMedian     float64
	FeeMean       float64
	FeeOwn        float64
	Balance       int64
	TicketPrice   int64
}

// Purchase is the main handler for purchasing tickets for the user.
// TODO Not make this an inlined pile of crap.
func (t *TicketPurchaser) Purchase(height int64) (*PurchaseStats, error) {

	ps := &PurchaseStats{Height: height}
	// Check to make sure that the current height has not already been seen
	// for a reorg or a fork
	if _, exists := t.heightCheck[height]; exists {
		log.Debugf("We've already seen this height, "+
			"reorg/fork detected at height %v", height)
		return ps, nil
	}
	t.heightCheck[height] = struct{}{}

	if t.cfg.TicketFeeInfo {
		oneBlock := uint32(1)
		info, err := t.dcrdChainSvr.TicketFeeInfo(&oneBlock, &zeroUint32)
		if err != nil {
			return ps, err
		}
		ps.FeeMin = info.FeeInfoBlocks[0].Min
		ps.FeeMax = info.FeeInfoBlocks[0].Max
		ps.FeeMedian = info.FeeInfoBlocks[0].Median
		ps.FeeMean = info.FeeInfoBlocks[0].Mean
	}

	// Just starting up, initialize our purchaser and start
	// buying. Set the start up regular transaction fee here
	// too.
	winSize := t.activeNet.StakeDiffWindowSize
	if t.firstStart {
		t.idxDiffPeriod = int(height % winSize)
		t.windowPeriod = int(height / winSize)
		t.firstStart = false

		log.Debugf("First run time, initialized idxDiffPeriod to %v",
			t.idxDiffPeriod)

		txFeeAmt, err := dcrutil.NewAmount(t.cfg.TxFee)
		if err != nil {
			log.Errorf("Failed to decode tx fee amount %v from config",
				t.cfg.TxFee)
		} else {
			t.wallet.SetRelayFee(txFeeAmt)
		}
	}

	// Move the respective cursors for our positions
	// in the blockchain.
	t.idxDiffPeriod = int(height % winSize)
	t.windowPeriod = int(height / winSize)

	// The general case initialization for this function. It
	// sets our index in the difficulty period, and then
	// decides if it needs to fill the queue with tickets to
	// purchase.
	// Check to see if we're in a new difficulty period.
	// Roll over all of our variables if this is true.
	if (height+1)%winSize == 0 {
		log.Debugf("Resetting stake window ticket variables "+
			"at height %v", height)
	}

	// We may have disconnected and reconnected in a
	// different window period. If this is the case,
	// we need reset our variables too.
	thisWindowPeriod := int(height / winSize)
	if (height+1)%winSize != 0 &&
		thisWindowPeriod > t.windowPeriod {
		log.Debugf("Detected assymetry in this window period versus "+
			"stored window period, resetting purchase orders at "+
			"height %v", height)
	}

	log.Debugf("The total blocks remaining in this diff period is %v.", int(winSize)-t.idxDiffPeriod)

	// Parse the ticket purchase frequency. Positive numbers mean
	// that many tickets per block. Negative numbers mean to only
	// purchase one ticket once every abs(num) blocks.
        // todo: throw an error if MaxPerBlock is set above 20
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

	// Make sure that our wallet is connected to the daemon and the
	// wallet is unlocked, otherwise abort.
	// TODO: Check daemon connected
	if t.wallet.Locked() {
		return ps, fmt.Errorf("Wallet not unlocked to allow ticket purchases")
	}

	avgPriceAmt, err := t.calcAverageTicketPrice(height)
	if err != nil {
		return ps, fmt.Errorf("Failed to calculate average ticket price amount: %s",
			err.Error())
	}
	avgPrice := avgPriceAmt.ToCoin()
	log.Debugf("Calculated average ticket price: %v", avgPriceAmt)
	ps.PriceAverage = avgPrice

	nextStakeDiff, err := t.wallet.StakeDifficulty()
	if err != nil {
		return ps, err
	}
	ps.PriceCurrent = float64(nextStakeDiff)
	t.ticketPrice = nextStakeDiff
	ps.TicketPrice = int64(nextStakeDiff)

	sDiffEsts, err := t.dcrdChainSvr.EstimateStakeDiff(nil)
	if err != nil {
		return ps, err
	}

	maxPriceAbsAmt, err := dcrutil.NewAmount(t.cfg.MaxPriceAbsolute)
	if err != nil {
		return ps, err
	}
	// a sensible default, 25% above avgprice
	negone, err := dcrutil.NewAmount(-1) 
	if maxPriceAbsAmt == negone {
		maxPriceAbsAmt, err = dcrutil.NewAmount(avgPrice * 1.25)
		if err != nil {
			return ps, err
		}
		log.Debugf("Set max price to %v", maxPriceAbsAmt)
	}

	// Scale the average price according to the configuration parameters
	// to find minimum and maximum prices for users that are electing to
	// attempting to manipulate the stake difficulty.

        /*
	maxPriceScaledAmt, err := dcrutil.NewAmount(t.cfg.MaxPriceScale * avgPrice)
	if err != nil {
		return ps, err
	}
	if t.maintainMaxPrice {
		log.Debugf("The maximum price to maintain for this round is set to %v",
			maxPriceScaledAmt)
	}
	ps.PriceMaxScale = maxPriceScaledAmt.ToCoin()

	minPriceScaledAmt, err := dcrutil.NewAmount(t.cfg.MinPriceScale * avgPrice)
	if err != nil {
		return ps, err
	}
	if t.maintainMinPrice {
		log.Debugf("The minimum price to maintain for this round is set to %v",
			minPriceScaledAmt)
	}
	ps.PriceMinScale = minPriceScaledAmt.ToCoin()
        */

	account, err := t.wallet.AccountNumber(t.cfg.AccountName)
	if err != nil {
		return ps, err
	}
	balSpendable, err := t.wallet.CalculateAccountBalance(account, 0, wtxmgr.BFBalanceSpendable)
	if err != nil {
		return ps, err
	}
	log.Debugf("Current spendable balance at height %v for account '%s': %v",
		height, t.cfg.AccountName, balSpendable)
	ps.Balance = int64(balSpendable)

	// Calculate the remaining tickets that could possibly be
	// mined in the current window.
	diffPeriod := int64(0)
	if (height+1)%winSize != 0 {
		// In the middle of a window
		diffPeriod = int64(t.idxDiffPeriod)
	}
	ticketsLeftInWindow := (winSize - diffPeriod) *
		int64(t.activeNet.MaxFreshStakePerBlock)

	log.Debugf("The total ticket allotment left in this stakediff window is %v. ", ticketsLeftInWindow)

	// Disable purchasing if the ticket price is too high based on
	// the absolute cutoff or if the estimated ticket price is above
	// our scaled cutoff based on the ideal ticket price.
	if nextStakeDiff > maxPriceAbsAmt {
		log.Debugf("Aborting ticket purchases because the ticket price %v "+
			"is higher than the maximum absolute price %v", nextStakeDiff,
			maxPriceAbsAmt)
		return ps, nil
	}
        /*
	if t.maintainMaxPrice && (sDiffEsts.Min > maxPriceScaledAmt.ToCoin()) &&
		maxPriceScaledAmt != 0 {
		log.Debugf("Aborting ticket purchases because the ticket price "+
			"next window estimate %v is higher than the maximum scaled "+
			"price %v", sDiffEsts.Expected, maxPriceScaledAmt)
		return ps, nil
	}
        */

	// If might be the case that there weren't enough recent
	// blocks to average fees from. Use data from the last
	// window with the closest difficulty.
	chainFee := 0.0
	if t.idxDiffPeriod < t.cfg.BlocksToAvg {
		chainFee, err = t.findClosestFeeWindows(nextStakeDiff.ToCoin(),
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

	// Scale the mean fee upwards according to what was asked
	// for by the user.
	feeToUse := chainFee * t.cfg.FeeTargetScaling
        // todo: calculate maxFee ROI here
	if feeToUse > t.cfg.MaxFee {
		log.Debugf("Scaled fee is %v, but max fee is %v; using max",
			feeToUse, t.cfg.MaxFee)
		feeToUse = t.cfg.MaxFee
	}
	if feeToUse < t.cfg.MinFee {
		log.Debugf("Scaled fee is %v, but min fee is %v; using min",
			feeToUse, t.cfg.MinFee)
		feeToUse = t.cfg.MinFee
	}
	feeToUseAmt, err := dcrutil.NewAmount(feeToUse)
	if err != nil {
		return ps, err
	}
	t.wallet.SetTicketFeeIncrement(feeToUseAmt)

	log.Debugf("Mean fee for the last blocks or window period was %v; "+
		"this was scaled to %v", chainFee, feeToUse)
	ps.FeeOwn = feeToUse

	// Calculate how many tickets we could buy at this difficulty.
	// note, this is using next stake diff, but this may not be correct because
	// it takes 2 blocks for a purchase to happen.
	toBuyDiffPeriod := int(float64(math.Floor((balSpendable.ToCoin()) / nextStakeDiff.ToCoin())))
	toBuyForBlock := toBuyDiffPeriod
	buyOpportunities := int(winSize) - 1 - t.idxDiffPeriod

	// make this a configurable option that can be enabled / disabled
	if !t.cfg.BuyImmediately {
		log.Debugf("Spreading purchases throughout window")
		if buyOpportunities > 0 && toBuyDiffPeriod > 0 {
			// the rand is for the remainder, so if you need to buy 5.25 tickets per block, then 1/4th of the time you will buy 6 tickets.
			rand.Seed(time.Now().UTC().UnixNano())
			log.Debugf("To spend all funds within this window we would need to buy %f tickets per block. (%d purchases)", float64(toBuyDiffPeriod)/float64(buyOpportunities), toBuyDiffPeriod)
			if toBuyDiffPeriod >= buyOpportunities {
				toBuyForBlock = int(math.Floor(float64(toBuyDiffPeriod) / float64(buyOpportunities)))
				if rand.Float64() <= float64(toBuyDiffPeriod%buyOpportunities)/float64(buyOpportunities) {
					toBuyForBlock++
				}
			} else {
				// spread throughout window
				if rand.Float64() <= float64(toBuyDiffPeriod)/float64(buyOpportunities) {
					log.Debugf("Buying one this round")
					toBuyForBlock = 1
				} else {
					toBuyForBlock = 0
					log.Debugf("Skipping this round")
				}
			}
		} else {
			toBuyForBlock = 0
			if toBuyDiffPeriod < 0 {
				log.Debugf("toBuyDiffPeriod < 0, this should not occur")
			} else if toBuyDiffPeriod == 0 {
				log.Debugf("Not enough funds to purchase a ticket")
			}
			if buyOpportunities < 0 {
				log.Debugf("buyOpportunities < 0, this should not occur")
			} else if buyOpportunities == 0 {
				log.Debugf("There are no more opportunities to purchase tickets this this window")
			}
		}
	}

	// Only the maximum number of tickets at each block
	// should be purchased, as specified by the user.
	if toBuyForBlock > maxPerBlock {
		toBuyForBlock = maxPerBlock
		log.Debugf("Limiting to %d purchases per block", maxPerBlock)
	}

	// minimum ticket price manipulation

	minPriceScaledAmt, err := dcrutil.NewAmount(t.cfg.MinPriceScale * avgPrice)
	if err != nil {
		return ps, err
	}

        // todo: max in mempool should be 40 here
        // todo: fee should be minimum 0.01

	if t.cfg.MinPriceScale > 0.0 {
		if sDiffEsts.Min < minPriceScaledAmt.ToCoin() {
			toBuyForBlock = maxPerBlock
			log.Debugf("Attempting to manipulate the stake difficulty "+
				"so that the price does not fall below the set minimum "+
				"%v (current minimum for next stake difficulty: %v) ",
				minPriceScaledAmt, sDiffEsts.Min)
		}
	}

	// We've already purchased all the tickets we need to.
	if toBuyForBlock < 0 {
		log.Debugf("toBuyForBlock < 0, this should not occur")
		return ps, nil
	} else if toBuyForBlock == 0 {
		log.Debugf("All tickets have been purchased")
		return ps, nil
	}

	// Check our balance versus the amount of tickets we need to buy.
	// If there is not enough money, decrement and recheck the balance
	// to see if fewer tickets may be purchased. Abort if we don't
	// have enough moneys.
	notEnough := func(bal dcrutil.Amount, toBuy int, sd dcrutil.Amount) bool {
		return (bal.ToCoin() - float64(toBuy)*sd.ToCoin()) <
			t.cfg.BalanceToMaintain
	}
	if notEnough(balSpendable, toBuyForBlock, nextStakeDiff) {
		for notEnough(balSpendable, toBuyForBlock, nextStakeDiff) {
			if toBuyForBlock == 0 {
				break
			}

			toBuyForBlock--
		}

		if toBuyForBlock == 0 {
			log.Debugf("Aborting purchasing of tickets because our balance "+
				"after buying tickets is estimated to be %v but balance "+
				"to maintain is set to %v",
				(balSpendable.ToCoin() - float64(toBuyForBlock)*
					nextStakeDiff.ToCoin()),
				t.cfg.BalanceToMaintain)
			return ps, nil
		}
	}

	// If an address wasn't passed, create an internal address in
	// the wallet for the ticket address.
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

	// Purchase tickets.
	poolFeesAmt, err := dcrutil.NewAmount(t.cfg.PoolFees)
	if err != nil {
		return ps, err
	}
	expiry := int32(int(height) + int(buyOpportunities/2) + 2)
	hashes, err := t.wallet.PurchaseTickets(0,
		maxPriceAbsAmt,
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
			"fees per KB used)", tickets[i], nextStakeDiff.ToCoin(),
			feeToUseAmt.ToCoin())
	}

	balSpendable, err = t.wallet.CalculateAccountBalance(account, 0,
		wtxmgr.BFBalanceSpendable)
	if err != nil {
		return ps, err
	}
	log.Debugf("Final spendable balance at height %v for account '%s' "+
		"after ticket purchases: %v", height, t.cfg.AccountName, balSpendable)
	ps.Balance = int64(balSpendable)

	return ps, nil
}
